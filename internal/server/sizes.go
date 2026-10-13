package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/sizing"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Fractional pool sizes (docs/design/sizes.md): a shaped pool's ladder, a
// Run's size request, and how both are persisted.

// PoolShape is an operator-declared nominal host shape; pools.shape.
type PoolShape struct {
	Family      string     `json:"family" doc:"The size family, a lowercase slug (e.g. m): the first part of every size name. Pool metadata, never inferred from the provider."`
	Arch        string     `json:"arch" enum:"arm64,amd64" doc:"The hosts' architecture, the last part of every size name."`
	CPUs        float64    `json:"cpus" doc:"Nominal CPUs of a whole host: a power of two."`
	Memory      spec.Bytes `json:"memory" doc:"Nominal memory of a whole host, in the terms Runs ask in."`
	Disk        spec.Bytes `json:"disk" doc:"Nominal disk of a whole host (from its launch template, not its instance type)."`
	MinCPUs     float64    `json:"minCpus" doc:"The smallest size's CPUs: a power of two, at least 0.5. The ladder halves the whole host down to it."`
	DefaultCPUs float64    `json:"defaultCpus" doc:"The CPUs of the size a Run that asks for no resources gets: one of the ladder's."`
}

// PoolSize is one entry of a shaped pool's ladder.
type PoolSize struct {
	Name     string     `json:"name" example:"m-2-arm64"`
	CPUs     float64    `json:"cpus"`
	Memory   spec.Bytes `json:"memory" doc:"Bytes."`
	Disk     spec.Bytes `json:"disk" doc:"Bytes."`
	Fraction float64    `json:"fraction" doc:"Of a whole host: 1, 0.5, 0.25, ..."`
	Default  bool       `json:"default,omitempty" doc:"The size a Run that asks for no resources gets."`
}

func (sh PoolShape) model() (*sizing.Model, error) {
	return sizing.New(sizing.Shape{
		Dimensions: sizing.Dimensions{CPUs: sh.CPUs, Memory: sh.Memory, Disk: sh.Disk},
		Family:     sh.Family, Arch: sh.Arch, MinCPUs: sh.MinCPUs, DefaultCPUs: sh.DefaultCPUs,
	})
}

// sizes is sh's ladder, smallest first; nil for a shape New refuses.
func (sh PoolShape) sizes() []PoolSize {
	m, err := sh.model()
	if err != nil {
		return nil
	}
	var out []PoolSize
	for _, e := range m.List() {
		out = append(out, PoolSize{Name: e.Name, CPUs: e.CPUs, Memory: e.Memory, Disk: e.Disk, Fraction: e.Fraction, Default: e.CPUs == sh.DefaultCPUs})
	}
	return out
}

// shapeKeys are ParseShape's keys, in the order its errors list them.
var shapeKeys = []string{"family", "arch", "cpus", "memory", "disk", "min-cpus", "default-cpus"}

// ParseShape reads `luxd admin create-pool --shape` and `lux pools set
// --shape`: family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2,
// every key required. Values are checked by ValidShape, not here.
func ParseShape(s string) (*PoolShape, error) {
	var sh PoolShape
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !slices.Contains(shapeKeys, k) {
			return nil, fmt.Errorf("shape %q: want %s=VALUE pairs", s, strings.Join(shapeKeys, "|"))
		}
		var err error
		switch k {
		case "family":
			sh.Family = v
		case "arch":
			sh.Arch = v
		case "cpus":
			sh.CPUs, err = strconv.ParseFloat(v, 64)
		case "min-cpus":
			sh.MinCPUs, err = strconv.ParseFloat(v, 64)
		case "default-cpus":
			sh.DefaultCPUs, err = strconv.ParseFloat(v, 64)
		case "memory":
			err = json.Unmarshal([]byte(strconv.Quote(v)), &sh.Memory)
		case "disk":
			err = json.Unmarshal([]byte(strconv.Quote(v)), &sh.Disk)
		}
		if err != nil {
			return nil, fmt.Errorf("shape %q: %s: invalid value %q", s, k, v)
		}
		seen[k] = true
	}
	var missing []string
	for _, k := range shapeKeys {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("shape %q: missing %s", s, strings.Join(missing, ", "))
	}
	return &sh, nil
}

// ValidShape is a 422 invalid_pool for a shape sizing.New refuses.
func ValidShape(sh PoolShape) error {
	if _, err := sh.model(); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
	}
	return nil
}

// CheckPoolShape validates a pool's shape (nil: none) and, for an ec2
// pool, its template's instanceType and fallbackInstanceTypes against it,
// as far as the instance-type catalog describes them: each must match the
// shape's architecture and have at least its CPUs and memory. Disk comes
// from the launch template, which every type shares, so it is not the
// type's to check. A type the catalog lacks is not checked.
func CheckPoolShape(ctx context.Context, db *store.Store, provider string, template map[string]any, sh *PoolShape) error {
	if sh == nil {
		return nil
	}
	if err := ValidShape(*sh); err != nil {
		return err
	}
	if provider != "ec2" {
		return nil
	}
	m, _ := sh.model()
	region, _ := template["region"].(string)
	primary, _ := template["instanceType"].(string)
	types := []string{primary}
	if list, ok := template["fallbackInstanceTypes"].([]any); ok {
		for _, v := range list {
			if t, ok := v.(string); ok {
				types = append(types, t)
			}
		}
	}
	if region == "" || primary == "" {
		return nil
	}
	return db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT instance_type, vcpus, memory_bytes, coalesce(architectures, '{}') FROM instance_types
			WHERE provider = $1 AND region = $2 AND instance_type = ANY($3)`, provider, region, types)
		if err != nil {
			return err
		}
		known := map[string]sizing.Dimensions{}
		archs := map[string]string{}
		for rows.Next() {
			var name string
			var vcpus *int
			var mem *int64
			var ec2 []string
			if err := rows.Scan(&name, &vcpus, &mem, &ec2); err != nil {
				return err
			}
			if vcpus == nil || mem == nil {
				continue
			}
			// The shape's arch when the type offers it, else the first it
			// offers; a type the catalog gives no arch is not refused for it.
			arch := ""
			for _, a := range ec2 {
				if g := ec2Archs[a]; g == sh.Arch || arch == "" {
					arch = g
				}
			}
			if len(ec2) == 0 {
				arch = sh.Arch
			}
			known[name] = sizing.Dimensions{CPUs: float64(*vcpus), Memory: spec.Bytes(*mem), Disk: sh.Disk}
			archs[name] = arch
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i, t := range types {
			d, ok := known[t]
			if !ok {
				continue
			}
			if err := m.ValidateFallback(archs[t], d); err != nil {
				field := "template.instanceType"
				if i > 0 {
					field = fmt.Sprintf("template.fallbackInstanceTypes[%d]", i-1)
				}
				return errf(http.StatusUnprocessableEntity, "invalid_pool", "invalid pool shape: %s %s: %s", field, t, err.Error())
			}
		}
		return nil
	})
}

// SizeRequest is what a Run asked for in resources, as the sizing model
// reads it: only the fields given (a zero is not given).
type SizeRequest struct {
	Size   string      `json:"size,omitempty"`
	CPUs   *float64    `json:"cpus,omitempty"`
	Memory *spec.Bytes `json:"memory,omitempty" doc:"Bytes."`
	Disk   *spec.Bytes `json:"disk,omitempty" doc:"Bytes."`
}

// SizeResolved is the size a Run has, and its resources.
type SizeResolved struct {
	Size   string     `json:"size"`
	CPUs   float64    `json:"cpus"`
	Memory spec.Bytes `json:"memory" doc:"Bytes."`
	Disk   spec.Bytes `json:"disk" doc:"Bytes."`
}

// RunSizing is a Run's request and resolution on a shaped pool.
type RunSizing struct {
	Requested *SizeRequest  `json:"requested,omitempty" doc:"What its submit asked for in resources.size, cpus, memory and disk, only those given (empty: none, the pool's default size). Absent for a Run submitted to an unshaped pool. A resume's request is in its answer's resize and its resume.requested event."`
	Resolved  *SizeResolved `json:"resolved,omitempty" doc:"The size the Run has now, and its cpus, memory and disk (as spec.resources). Absent while it is in an unshaped pool."`
}

// sizeRequestOf is r's size request: its size, cpus, memory and disk
// that are not zero.
func sizeRequestOf(r spec.Resources) SizeRequest {
	var q SizeRequest
	q.Size = r.Size
	if r.CPUs != 0 {
		q.CPUs = &r.CPUs
	}
	if r.Memory != 0 {
		q.Memory = &r.Memory
	}
	if r.Disk != 0 {
		q.Disk = &r.Disk
	}
	return q
}

func (q SizeRequest) model() sizing.Request {
	var r sizing.Request
	r.Size = sizing.Optional[string]{Present: q.Size != "", Value: q.Size}
	if q.CPUs != nil {
		r.CPUs = sizing.Optional[float64]{Present: true, Value: *q.CPUs}
	}
	if q.Memory != nil {
		r.Memory = sizing.Optional[spec.Bytes]{Present: true, Value: *q.Memory}
	}
	if q.Disk != nil {
		r.Disk = sizing.Optional[spec.Bytes]{Present: true, Value: *q.Disk}
	}
	return r
}

func (q SizeRequest) equal(o SizeRequest) bool {
	eq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	eqb := func(a, b *spec.Bytes) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return q.Size == o.Size && eq(q.CPUs, o.CPUs) && eqb(q.Memory, o.Memory) && eqb(q.Disk, o.Disk)
}

// sizingError is the API error for a sizing.Resolve failure in pool.
func sizingError(pool string, err error) error {
	var over *sizing.OversizedError
	switch {
	case errors.As(err, &over):
		return errf(http.StatusUnprocessableEntity, "size_too_large", "pool %s: %s", pool, err.Error())
	case errors.Is(err, sizing.ErrConflict):
		return errf(http.StatusUnprocessableEntity, "size_conflict", "pool %s: resources.size cannot be combined with cpus, memory or disk", pool)
	case errors.Is(err, sizing.ErrUnknownSize):
		return errf(http.StatusUnprocessableEntity, "unknown_size", "pool %s: %s", pool, err.Error())
	case errors.Is(err, sizing.ErrInvalidRequest):
		return invalidSpec(&spec.ValidationError{Problems: []string{"resources: cpus, memory and disk must be greater than 0"}})
	}
	return err
}

func errSizeUnsupported(pool string) error {
	return errf(http.StatusUnprocessableEntity, "size_unsupported", "pool %s has no shape, so no sizes: ask with resources.cpus, memory and disk", pool)
}

// poolShape is pool id's shape; nil for an unshaped pool or none.
func poolShape(ctx context.Context, tx pgx.Tx, poolID *string) (*PoolShape, error) {
	if poolID == nil || *poolID == "" {
		return nil, nil
	}
	var sh *PoolShape
	err := tx.QueryRow(ctx, `SELECT shape FROM pools WHERE id = $1`, *poolID).Scan(&sh)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return sh, err
}

// resolveSubmitSize resolves a submit's resources as asked (raw: before
// luxd's defaults) in its pool. An unshaped pool refuses a size and leaves
// res as Normalize filled it; a shaped one sets res's cpus, memory and
// disk to the size it resolves to (pids are not the size's). The returned
// request and size are nil for an unshaped pool.
func resolveSubmitSize(ctx context.Context, tx pgx.Tx, rp resolvedPool, raw spec.Resources, res *spec.Resources) (*SizeRequest, *string, error) {
	sh, err := poolShape(ctx, tx, rp.ID)
	if err != nil {
		return nil, nil, err
	}
	if sh == nil {
		if raw.Size != "" {
			return nil, nil, errSizeUnsupported(rp.Name)
		}
		return nil, nil, nil
	}
	m, err := sh.model()
	if err != nil {
		return nil, nil, err
	}
	req := sizeRequestOf(raw)
	got, err := m.Resolve(req.model())
	if err != nil {
		return nil, nil, sizingError(rp.Name, err)
	}
	res.CPUs, res.Memory, res.Disk = got.Resolved.CPUs, got.Resolved.Memory, got.Resolved.Disk
	return &req, &got.Resolved.Name, nil
}

// checkReplay refuses an idempotent re-submit whose resources differ from
// the shaped Run's submit asked for (requested, never resolved: two
// requests that round to one size are still different requests). A Run
// submitted to an unshaped pool records no request, and is returned as it
// always was.
func checkReplay(existing *Run, raw spec.Resources) error {
	if existing.Sizing == nil || existing.Sizing.Requested == nil {
		return nil
	}
	if !existing.Sizing.Requested.equal(sizeRequestOf(raw)) {
		return errf(http.StatusConflict, "idempotency_conflict",
			"Idempotency-Key was used for Run %s, submitted with other resources", existing.ID)
	}
	return nil
}

// savedDisk is the peak disk use of the placement that took the snapshot
// the Run resumes from, and whether that placement reported its final
// sample (exited). The runner reports the snapshot before the exit status
// carrying its final disk sample, and only that status makes a placement
// 'exited'. In any other state (lost between the two) the peak may
// predate data the snapshot holds.
func savedDisk(ctx context.Context, tx pgx.Tx, runID string) (peak *int64, final, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT p.peak_disk_bytes, p.state = 'exited' FROM runs r
		JOIN snapshots sn ON sn.id = r.snapshot_id AND sn.run_id = r.id
		JOIN placements p ON p.id = sn.placement_id
		WHERE r.id = $1`, runID).Scan(&peak, &final)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	return peak, final, err == nil, err
}

// resizeShaped resolves a resume's request r in the shaped pool (its
// model m) and writes the size into the Run: its spec's cpus, memory and
// disk and its resolved_size. cur is what the Run has; asked is what the
// resume asked for (zero for a pool move that asked for nothing). A size
// with less disk than the Run has must still hold its saved state with
// the shrink headroom unshaped pools use; otherwise it is refused, never
// kept at the old disk, which would break the size's coupling.
func resizeShaped(ctx context.Context, tx pgx.Tx, runID, pool string, cur spec.Resources, m *sizing.Model, asked spec.Resources, r sizing.Request) (*Resize, error) {
	got, err := m.Resolve(r)
	if err != nil {
		return nil, sizingError(pool, err)
	}
	e := got.Resolved
	rz := &Resize{Requested: asked, Applied: cur, Size: e.Name}
	rz.Applied.CPUs, rz.Applied.Memory, rz.Applied.Disk, rz.Applied.Size = e.CPUs, e.Memory, e.Disk, ""
	if e.Disk < cur.Disk {
		// TODO(sizes phase 4): round up to the smallest size whose disk
		// fits the saved state, instead of refusing.
		peak, final, found, err := savedDisk(ctx, tx, runID)
		if err != nil {
			return nil, err
		}
		switch {
		case found && !final:
			return nil, errSizeBelowState(e, "the placement that took the snapshot it resumes from ended without reporting its final disk use")
		case peak == nil:
			return nil, errSizeBelowState(e, "no disk use is recorded for the snapshot it resumes from")
		case int64(e.Disk) < diskShrinkFloor(*peak):
			return nil, errSizeBelowState(e, fmt.Sprintf("its saved state used up to %s, which needs at least %s of disk (that plus max(25%%, 1 GiB))",
				bytesText(*peak), bytesText(diskShrinkFloor(*peak))))
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET resolved_size = $3,
			spec = jsonb_set(spec, '{resources}', coalesce(spec->'resources', '{}') || $2::jsonb) WHERE id = $1`,
		runID, map[string]any{"cpus": rz.Applied.CPUs, "memory": rz.Applied.Memory, "disk": rz.Applied.Disk}, e.Name); err != nil {
		return nil, err
	}
	return rz, nil
}

func errSizeBelowState(e sizing.Entry, why string) error {
	return errf(http.StatusUnprocessableEntity, "size_below_saved_state",
		"size %s has %s of disk, less than the Run has now: %s; a shaped pool never keeps a larger disk than its size, so choose a size with more disk",
		e.Name, bytesText(int64(e.Disk)), why)
}

// resumeSize applies a resume's resources and pool move to a Run's size,
// against the destination pool dest (its shape sh, nil: unshaped), before
// the move itself (rebindPool) checks the Run fits there. moving: dest is
// another pool than the Run's. An unshaped destination keeps today's
// rules (resizeRun) and drops the Run's size; a shaped one resolves what
// the resume asked, or on a move that asked nothing the Run's current
// cpus, memory and disk, rounded up. resolved is the Run's size now ("":
// none). nil, nil: nothing to change.
func resumeSize(ctx context.Context, tx pgx.Tx, runID, dest string, sh *PoolShape, moving bool, resolved string, cur spec.Resources, req *resumeResources) (*Resize, error) {
	var asked spec.Resources
	if req != nil {
		var err error
		if asked, err = req.check(); err != nil {
			return nil, err
		}
	}
	if sh == nil {
		if asked.Size != "" {
			return nil, errSizeUnsupported(dest)
		}
		if resolved != "" && (moving || asked != (spec.Resources{})) {
			if _, err := tx.Exec(ctx, `UPDATE runs SET resolved_size = NULL WHERE id = $1`, runID); err != nil {
				return nil, err
			}
		}
		if req == nil {
			return nil, nil
		}
		return resizeRun(ctx, tx, runID, cur, *req)
	}
	m, err := sh.model()
	if err != nil {
		return nil, err
	}
	switch {
	case asked != (spec.Resources{}):
		return resizeShaped(ctx, tx, runID, dest, cur, m, asked, sizeRequestOf(asked).model())
	case moving:
		raw := sizeRequestOf(spec.Resources{CPUs: cur.CPUs, Memory: cur.Memory, Disk: cur.Disk})
		return resizeShaped(ctx, tx, runID, dest, cur, m, asked, raw.model())
	case req != nil:
		return resizeRun(ctx, tx, runID, cur, *req)
	}
	return nil, nil
}
