package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

type resumeTarget struct {
	PoolID string `json:"poolId" doc:"Move the Run to this pool from this resume on, by id: the same field and rule as a submit's placement.poolId (the tenant's active pool with that id, else the platform's; any other is 422 unknown_pool). The Run's pool (pool and poolId in GET) and its spec's placement.pool and placement.poolId become that pool's, so later resumes, retries and migrations stay there. Checked before anything changes, with resources as this resume sets them: 422 platform_mismatch when the platforms the image offered at its last placement and the pool's hosts' are both known and share none; 422 pool_unfit when no host the pool has or launches can ever take the Run (nested containers, placement.requires labels, cpus, memory or disk beyond a whole host). On a Run already resuming: its own pool, 202; another, 409 not_resumable."`
}

// BoundPool names a pool by its current name and its id.
type BoundPool struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// ResumePlacement is, in a resume's answer that named a pool, where the
// Run is bound from now on.
type ResumePlacement struct {
	Pool              string     `json:"pool" doc:"The pool's current name."`
	PoolID            string     `json:"poolId"`
	From              *BoundPool `json:"from,omitempty" doc:"The pool the Run left; absent when it was already there."`
	PlatformUnchecked bool       `json:"platformUnchecked,omitempty" doc:"The Run moved without its platform being checked: lux does not know the platforms its image offers (no placement reported them) or those of the pool's hosts (none registered, and the catalog lacks its template's instance types), or either."`
}

// poolProfile is one kind of host a pool has or will launch: a host of it,
// or (ec2) the host its current template launches. A zero capacity field
// is unknown, as hostFit reads it: no limit.
type poolProfile struct {
	Labels      map[string]string
	Capacity    proto.Capacity
	LabelsKnown bool
}

// poolFacts is what lux knows of the hosts a pool can give a Run.
type poolFacts struct {
	Name     string
	Provider string
	// Archs: GOARCH names of its hosts' and template's platforms; empty
	// when none is known.
	Archs    []string
	Profiles []poolProfile
	// TemplateNested: an ec2 template's nestedContainers opt-in.
	TemplateNested bool
}

// ec2Archs maps EC2's architecture names (DescribeInstanceTypes) to Go's,
// which runners report.
var ec2Archs = map[string]string{"x86_64": "amd64", "arm64": "arm64", "i386": "386"}

// loadPoolFacts reads, as the system (instance_types is no tenant's), what
// pool poolID offers a Run of tenantID: its live hosts and its latest
// expectationWindow registrations (for ec2 those of its current template,
// poolHostSizes' rule), and for ec2 the host its template launches, sized
// and platformed from the instance-type catalog.
func (s *Server) loadPoolFacts(ctx context.Context, poolID, tenantID string) (poolFacts, error) {
	var f poolFacts
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var template json.RawMessage
		var region string
		err := tx.QueryRow(ctx, `SELECT name, provider, coalesce(template, '{}'), coalesce(template->>'region', '') FROM pools WHERE id = $1`, poolID).
			Scan(&f.Name, &f.Provider, &template, &region)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT h.arch, h.labels, h.capacity, h.state IN ('ready', 'draining') AND h.launch_template IS DISTINCT FROM p.template
			FROM hosts h JOIN pools p ON p.id = h.pool_id
			WHERE h.pool_id = $1 AND (h.tenant_id IS NULL OR h.tenant_id = $2) AND h.registered_at IS NOT NULL
			  AND (h.state IN ('ready', 'draining') OR h.id IN (SELECT r.id FROM hosts r
					WHERE r.pool_id = $1 AND (r.tenant_id IS NULL OR r.tenant_id = $2) AND r.registered_at IS NOT NULL
					  AND (p.provider = 'static' OR r.provision_requested_at IS NOT NULL AND r.launch_template = p.template)
					ORDER BY r.registered_at DESC, r.id DESC LIMIT $3))`, poolID, tenantID, expectationWindow)
		if err != nil {
			return err
		}
		var observed *hostExpectation // the current template's registrations
		var observedMax proto.Capacity
		for rows.Next() {
			var arch string
			var labels map[string]string
			var capacity proto.Capacity
			var olderTemplate bool
			if err := rows.Scan(&arch, &labels, &capacity, &olderTemplate); err != nil {
				rows.Close()
				return err
			}
			if arch != "" && !slices.Contains(f.Archs, arch) {
				f.Archs = append(f.Archs, arch)
			}
			f.Profiles = append(f.Profiles, poolProfile{Labels: labels, Capacity: capacity, LabelsKnown: true})
			if f.Provider == "ec2" && !olderTemplate {
				if observed == nil {
					observedMax = capacity
				}
				observed = observed.observe(capacity, cloneLabels(labels))
				observedMax = maxCapacity(observedMax, capacity)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if f.Provider != "ec2" {
			return nil
		}
		f.TemplateNested = templateOffersNested(poolRow{Provider: f.Provider, Template: template})
		future := poolProfile{Labels: map[string]string{}}
		if observed != nil {
			future.Labels, future.LabelsKnown, future.Capacity = observed.Labels, true, observedMax
		}
		delete(future.Labels, "nested")
		if f.TemplateNested {
			future.Labels["nested"] = "true"
		}
		var t struct {
			InstanceType          string   `json:"instanceType"`
			FallbackInstanceTypes []string `json:"fallbackInstanceTypes"`
		}
		_ = json.Unmarshal(template, &t)
		if t.InstanceType != "" && region != "" {
			types := append([]string{t.InstanceType}, t.FallbackInstanceTypes...)
			rows, err := tx.Query(ctx, `SELECT vcpus, memory_bytes, coalesce(architectures, '{}') FROM instance_types
				WHERE provider = $1 AND region = $2 AND instance_type = ANY($3)`, f.Provider, region, types)
			if err != nil {
				return err
			}
			var catalogCPUs float64
			var catalogMem int64
			sized := 0
			for rows.Next() {
				var vcpus *int
				var mem *int64
				var archs []string
				if err := rows.Scan(&vcpus, &mem, &archs); err != nil {
					rows.Close()
					return err
				}
				for _, a := range archs {
					if g := ec2Archs[a]; g != "" && !slices.Contains(f.Archs, g) {
						f.Archs = append(f.Archs, g)
					}
				}
				if vcpus != nil && mem != nil {
					sized++
					catalogCPUs, catalogMem = max(catalogCPUs, float64(*vcpus)), max(catalogMem, *mem)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			// The catalog bounds the template's host only when it sizes
			// every candidate type; disk is never the instance type's. A
			// registration without a limit (0) keeps none.
			if sized == len(types) && sized > 0 {
				if observed == nil || future.Capacity.CPUs != 0 {
					future.Capacity.CPUs = max(future.Capacity.CPUs, catalogCPUs)
				}
				if observed == nil || future.Capacity.Memory != 0 {
					future.Capacity.Memory = max(future.Capacity.Memory, catalogMem)
				}
			}
		}
		f.Profiles = append(f.Profiles, future)
		return nil
	})
	slices.Sort(f.Archs)
	return f, err
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// maxCapacity is the larger of each resource, 0 (no limit) winning.
func maxCapacity(a, b proto.Capacity) proto.Capacity {
	pick := func(x, y float64) float64 {
		if x == 0 || y == 0 {
			return 0
		}
		return max(x, y)
	}
	return proto.Capacity{CPUs: pick(a.CPUs, b.CPUs), Memory: int64(pick(float64(a.Memory), float64(b.Memory))),
		Disk: int64(pick(float64(a.Disk), float64(b.Disk))), Runs: int(pick(float64(a.Runs), float64(b.Runs)))}
}

// lastImagePlatforms are the image platforms of the Run's latest placement
// that reported any; nil when none did.
func lastImagePlatforms(ctx context.Context, tx pgx.Tx, runID string) ([]string, error) {
	var ps []string
	err := tx.QueryRow(ctx, `SELECT image_platforms FROM placements
		WHERE run_id = $1 AND image_platforms IS NOT NULL ORDER BY epoch DESC LIMIT 1`, runID).Scan(&ps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ps, err
}

// checkPlatform refuses a move to a pool none of whose platforms the image
// offers. Unknown on either side is no refusal: unchecked reports it.
func checkPlatform(f poolFacts, imagePlatforms []string) (unchecked bool, err error) {
	var imageArchs []string
	for _, p := range imagePlatforms {
		parts := strings.Split(p, "/")
		if len(parts) >= 2 && parts[0] == "linux" {
			imageArchs = append(imageArchs, parts[1])
		}
	}
	if len(imagePlatforms) == 0 || len(f.Archs) == 0 {
		return true, nil
	}
	for _, a := range imageArchs {
		if slices.Contains(f.Archs, a) {
			return false, nil
		}
	}
	runs := make([]string, len(f.Archs))
	for i, a := range f.Archs {
		runs[i] = "linux/" + a
	}
	return false, errf(http.StatusUnprocessableEntity, "platform_mismatch", "pool %s runs %s; this Run's image offers only %s",
		f.Name, strings.Join(runs, ", "), strings.Join(imagePlatforms, ", "))
}

// checkPoolFit refuses a move to a pool where no host it has or launches
// could ever take the Run, empty: hostFit's pool-decided blockers (nested
// containers, required labels, resources) on each profile. A pool lux
// knows nothing of (a static pool no host registered in) takes it.
func checkPoolFit(f poolFacts, poolID, tenantID string, sp spec.RunSpec) error {
	if len(f.Profiles) == 0 {
		return nil
	}
	r := pendingRun{TenantID: tenantID, Spec: sp, PoolID: &poolID}
	var best []fitBlocker
	for i, pr := range f.Profiles {
		h := &candidateHost{ID: fmt.Sprint("profile-", i), PoolID: poolID, Pool: f.Name, Labels: pr.Labels, Capacity: pr.Capacity,
			Connected: true, Caps: []string{proto.CapSyncModes}}
		if h.Labels == nil {
			h.Labels = map[string]string{}
		}
		blockers := slices.DeleteFunc(hostFit(r, h), func(b fitBlocker) bool {
			switch b.Kind {
			case kindNested, kindResource:
				return false
			case kindLabel:
				return !pr.LabelsKnown
			}
			return true
		})
		if len(blockers) == 0 {
			return nil
		}
		if best == nil || len(blockers) < len(best) {
			best = blockers
		}
	}
	var why []string
	for _, b := range best {
		switch b.Kind {
		case kindNested:
			if f.Provider == "ec2" {
				why = append(why, "it does not offer nested containers (its template has no nestedContainers, and none of its hosts runs lux-runner --nested)")
			} else {
				why = append(why, "none of its hosts offers nested containers (lux-runner --nested)")
			}
		case kindLabel:
			why = append(why, "none of its hosts has the labels placement.requires names")
		case kindResource:
			largest := 0.0
			for _, pr := range f.Profiles {
				switch b.Resource {
				case "cpus":
					largest = max(largest, pr.Capacity.CPUs)
				case "memory":
					largest = max(largest, float64(pr.Capacity.Memory))
				case "disk":
					largest = max(largest, float64(pr.Capacity.Disk))
				}
			}
			if b.Resource == "cpus" {
				why = append(why, fmt.Sprintf("it needs %g cpus; its largest host has %g", b.Requested, largest))
			} else {
				why = append(why, fmt.Sprintf("it needs %s of %s; its largest host has %s", bytesText(int64(b.Requested)), b.Resource, bytesText(int64(largest))))
			}
		}
	}
	slices.Sort(why)
	why = slices.Compact(why)
	return errf(http.StatusUnprocessableEntity, "pool_unfit", "pool %s cannot take this Run: %s", f.Name, strings.Join(why, "; "))
}

// rebindPool moves a resumed Run (its spec sp, resources as this resume
// sets them) from pool from to pool to, after checking that to can take
// it: its platform, then its fit. A refusal changes nothing (the caller's
// transaction rolls back). Already in to: nothing to check or change.
func (s *Server) rebindPool(ctx context.Context, tx pgx.Tx, tenantID, runID string, sp spec.RunSpec, from BoundPool, to resolvedPool, f *poolFacts) (*ResumePlacement, error) {
	moved := &ResumePlacement{Pool: to.Name, PoolID: *to.ID}
	if *to.ID == from.ID {
		return moved, nil
	}
	platforms, err := lastImagePlatforms(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	f.Name = to.Name
	if moved.PlatformUnchecked, err = checkPlatform(*f, platforms); err != nil {
		return nil, err
	}
	if err := checkPoolFit(*f, *to.ID, tenantID, sp); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET pool_id = $2,
			spec = jsonb_set(spec, '{placement}', coalesce(spec->'placement', '{}') || jsonb_build_object('pool', $3::text, 'poolId', $2::text))
		WHERE id = $1`, runID, *to.ID, to.Name); err != nil {
		return nil, err
	}
	moved.From = &from
	return moved, nil
}

// pendingPool is the pool move of the resume a resuming Run waits on
// (pendingResize's event); nil when it moved none.
func pendingPool(ctx context.Context, tx pgx.Tx, runID string) (*ResumePlacement, error) {
	var ev *struct {
		From              BoundPool `json:"from"`
		To                BoundPool `json:"to"`
		PlatformUnchecked bool      `json:"platformUnchecked"`
	}
	err := tx.QueryRow(ctx, `SELECT data->'pool' FROM run_events
		WHERE run_id = $1 AND type = 'resume.requested'
		  AND id > coalesce((SELECT max(id) FROM run_events WHERE run_id = $1 AND type = 'state' AND data->>'state' <> 'resuming'), 0)
		ORDER BY id DESC LIMIT 1`, runID).Scan(&ev)
	if errors.Is(err, pgx.ErrNoRows) || ev == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	from := ev.From
	return &ResumePlacement{Pool: ev.To.Name, PoolID: ev.To.ID, From: &from, PlatformUnchecked: ev.PlatformUnchecked}, nil
}
