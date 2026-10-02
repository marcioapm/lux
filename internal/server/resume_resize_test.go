package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// stoppedRun submits a Run of tenant t1 with res to pool "default", then
// stops it as a placement on host "ha" that reported snapshot "snap1"
// would: placement 1 exited with peakDisk (nil: no usage recorded), the
// snapshot on ha, uploaded. Returns its id.
func stoppedRun(t *testing.T, s *Server, res spec.Resources, peakDisk *int64) string {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	namedPools(t, s, "default")
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Volumes:   []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}},
		Resources: res,
		Placement: spec.Placement{Pool: "default"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', 'default', 'ready', '{"cpus":4,"memory":8589934592}', now()) ON CONFLICT DO NOTHING`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, peak_disk_bytes)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'exited', '{}', $2)`, id, peakDisk)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap-'||$1, 't1', $1, 'p-'||$1, 1, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', current_epoch = 1, snapshot_id = 'snap-'||id, pool_id = 'default' WHERE id = $1`, id)
	return id
}

func resumeWith(s *Server, id string, r *resumeResources) (*resumeOutput, error) {
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Resources: r}})
}

// storedResources is the Run's spec.resources in the database.
func storedResources(t *testing.T, s *Server, id string) spec.Resources {
	t.Helper()
	var r spec.Resources
	systemScan(t, s, `SELECT spec->'resources' FROM runs WHERE id = $1`, []any{id}, &r)
	return r
}

// resumeEvent is the data of the Run's latest resume.requested event.
func resumeEvent(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	var raw []byte
	systemScan(t, s, `SELECT data FROM run_events WHERE run_id = $1 AND type = 'resume.requested' ORDER BY id DESC LIMIT 1`, []any{id}, &raw)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func f64(v float64) *float64       { return &v }
func bytesPtr(v int64) *spec.Bytes { b := spec.Bytes(v); return &b }
func i64(v int64) *int64           { return &v }
func eventRes(m map[string]any, k string) map[string]any {
	res, _ := m["resources"].(map[string]any)
	sub, _ := res[k].(map[string]any)
	return sub
}

// cpus and memory change both ways: the stored spec, GET's view, the
// resume's answer and its resume.requested event all carry the new values;
// disk and pids stay as they were.
func TestResumeResizesCPUsAndMemory(t *testing.T) {
	for _, c := range []struct {
		name   string
		cpus   float64
		memory int64
	}{
		{"up", 4, 16 * gib},
		{"down", 0.5, gib / 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, i64(gib))
			out, err := resumeWith(s, id, &resumeResources{CPUs: f64(c.cpus), Memory: bytesPtr(c.memory)})
			if err != nil {
				t.Fatal(err)
			}
			want := spec.Resources{CPUs: c.cpus, Memory: spec.Bytes(c.memory), Disk: spec.Bytes(20 * gib), Pids: 1024}
			if got := storedResources(t, s, id); got != want {
				t.Errorf("stored %+v, want %+v", got, want)
			}
			got, err := s.getRun(tenantCtx("t1"), &RunPath{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if got.Body.Spec.Resources != want || got.Body.State != StateResuming {
				t.Errorf("GET: %s %+v, want resuming %+v", got.Body.State, got.Body.Spec.Resources, want)
			}
			if rz := out.Body.Resize; rz == nil || rz.Applied != want || rz.Disk != nil ||
				rz.Requested != (spec.Resources{CPUs: c.cpus, Memory: spec.Bytes(c.memory)}) {
				t.Errorf("answer's resize %+v", rz)
			}
			ev := resumeEvent(t, s, id)
			req, app := eventRes(ev, "requested"), eventRes(ev, "applied")
			if req["cpus"] != c.cpus || req["memory"] != float64(c.memory) || req["disk"] != nil {
				t.Errorf("event requested %v", req)
			}
			if app["cpus"] != c.cpus || app["memory"] != float64(c.memory) || app["disk"] != float64(20*gib) {
				t.Errorf("event applied %v", app)
			}
		})
	}
}

// Values submit would refuse are refused the same way (422 invalid_spec),
// and the Run stays as it was: stopped, its spec unchanged.
func TestResumeRefusesInvalidResources(t *testing.T) {
	for _, c := range []struct {
		name string
		r    resumeResources
	}{
		{"zero cpus", resumeResources{CPUs: f64(0)}},
		{"negative cpus", resumeResources{CPUs: f64(-1)}},
		{"zero memory", resumeResources{Memory: bytesPtr(0)}},
		{"negative memory", resumeResources{Memory: bytesPtr(-1)}},
		{"negative disk", resumeResources{Disk: -1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			before := spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}
			id := stoppedRun(t, s, before, i64(gib))
			_, err := resumeWith(s, id, &c.r)
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" || len(he.Details) == 0 {
				t.Fatalf("resume: %v, want 422 invalid_spec with details", err)
			}
			before.Pids = 1024
			var state string
			systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{id}, &state)
			if got := storedResources(t, s, id); got != before || state != StateStopped {
				t.Errorf("after refusal: %s %+v, want stopped %+v", state, got, before)
			}
		})
	}
	// Submit's own answer for a negative resource, for the shape.
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	_, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: spec.RunSpec{Image: spec.Image{Ref: "alpine"},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}}, Resources: spec.Resources{CPUs: -1}}})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" {
		t.Fatalf("submit: %v", err)
	}
}

// Disk: larger is applied; smaller is applied down to the snapshot's
// placement's peak use plus max(25%, 1 GiB), and otherwise kept, the Run
// resuming anyway, the answer and event saying why.
func TestResumeDisk(t *testing.T) {
	for _, c := range []struct {
		name      string
		peak      *int64
		requested int64
		applied   int64
		kept      bool
	}{
		{"grow", i64(gib), 40 * gib, 40 * gib, false},
		{"shrink, 1 GiB headroom fits", i64(2 * gib), 3 * gib, 3 * gib, false},
		{"shrink, 25% headroom fits", i64(8 * gib), 10 * gib, 10 * gib, false},
		{"shrink below 1 GiB headroom", i64(2 * gib), 3*gib - 1, 20 * gib, true},
		{"shrink below 25% headroom", i64(8 * gib), 10*gib - 1, 20 * gib, true},
		{"shrink without a measurement", nil, 10 * gib, 20 * gib, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, c.peak)
			out, err := resumeWith(s, id, &resumeResources{Disk: spec.Bytes(c.requested)})
			if err != nil {
				t.Fatal(err)
			}
			var state string
			systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{id}, &state)
			if got := storedResources(t, s, id).Disk; int64(got) != c.applied || state != StateResuming {
				t.Errorf("%s with disk %d, want resuming with %d", state, got, c.applied)
			}
			if got := out.Body.Spec.Resources.Disk; int64(got) != c.applied {
				t.Errorf("answer's spec disk %d, want %d", got, c.applied)
			}
			rz := out.Body.Resize
			if rz == nil || int64(rz.Applied.Disk) != c.applied || int64(rz.Requested.Disk) != c.requested || (rz.Disk != nil) != c.kept {
				t.Fatalf("answer's resize %+v", rz)
			}
			evDisk := eventRes(resumeEvent(t, s, id), "disk")
			if !c.kept {
				if evDisk != nil {
					t.Errorf("event says disk kept: %v", evDisk)
				}
				return
			}
			if int64(rz.Disk.Kept) != 20*gib || int64(rz.Disk.Requested) != c.requested || rz.Disk.Reason == "" {
				t.Errorf("answer's disk %+v", rz.Disk)
			}
			if c.peak != nil && (rz.Disk.Measured == nil || *rz.Disk.Measured != *c.peak || rz.Disk.Needed <= c.requested) {
				t.Errorf("answer's disk measurement %+v", rz.Disk)
			}
			if evDisk["requested"] != float64(c.requested) || evDisk["kept"] != float64(20*gib) || evDisk["reason"] != rz.Disk.Reason {
				t.Errorf("event's disk %v", evDisk)
			}
		})
	}
}

// The measurement is the placement that took the snapshot the Run resumes
// from: fromSnapshot's, not the latest.
func TestResumeDiskShrinkMeasuresFromSnapshot(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, i64(gib))
	// A later placement used 15 GiB and took the latest snapshot.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, peak_disk_bytes)
		VALUES ('p2', 't1', $1, 'ha', 2, 'exited', $2)`, id, 15*gib)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap2', 't1', $1, 'p2', 2, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET snapshot_id = 'snap2', current_epoch = 2 WHERE id = $1`, id)

	if _, err := resumeWith(s, id, &resumeResources{Disk: spec.Bytes(4 * gib)}); err != nil {
		t.Fatal(err)
	}
	if got := storedResources(t, s, id).Disk; int64(got) != 20*gib {
		t.Fatalf("from the latest snapshot (15 GiB used): disk %d, want kept", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{
		FromSnapshot: "snap-" + id, Resources: &resumeResources{Disk: spec.Bytes(4 * gib)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedResources(t, s, id).Disk; int64(got) != 4*gib {
		t.Fatalf("from the older snapshot (1 GiB used): disk %d, want 4 GiB", got)
	}
}

// A Run resume does not accept is refused as before, its spec unchanged.
// One already resuming takes a retry of the same sizes, and refuses others.
func TestResumeResizeOfANonResumableRun(t *testing.T) {
	t.Run("resuming", func(t *testing.T) {
		s := testServer(t)
		id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, nil)
		r := &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(10 * gib)}
		for i := range 2 {
			if _, err := resumeWith(s, id, r); err != nil {
				t.Fatalf("resume %d: %v", i, err)
			}
		}
		for _, other := range []*resumeResources{{CPUs: f64(2)}, {Memory: bytesPtr(2 * gib)}, {Disk: spec.Bytes(40 * gib)}} {
			_, err := resumeWith(s, id, other)
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_resumable" {
				t.Fatalf("resume with %+v while resuming: %v, want 409 not_resumable", other, err)
			}
		}
		want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(20 * gib), Pids: 1024}
		if got := storedResources(t, s, id); got != want {
			t.Errorf("spec %+v, want %+v", got, want)
		}
	})
	for _, state := range []string{StateRunning, StateSucceeded, StateCancelled} {
		t.Run(state, func(t *testing.T) {
			s := testServer(t)
			before := spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}
			id := stoppedRun(t, s, before, i64(gib))
			execSQL(t, s, context.Background(), `UPDATE runs SET state = $2 WHERE id = $1`, id, state)
			_, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib)})
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_resumable" {
				t.Fatalf("resume: %v, want 409 not_resumable", err)
			}
			before.Pids = 1024
			if got := storedResources(t, s, id); got != before {
				t.Errorf("spec %+v, want %+v", got, before)
			}
		})
	}
}

// A resized resume is reserved and placed at its new size. Smaller, it
// stays on its snapshot's host. Larger than that host has free, it goes to
// another host of its pool that fits; with none, it waits for capacity,
// saying so, exactly as a submit of that size would.
func TestResumeResizedPlacement(t *testing.T) {
	setup := func(t *testing.T) (*Server, string) {
		s := testServer(t)
		s.cfg.LeaseDuration = time.Minute
		id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, i64(gib))
		s.hub.polled("ha")
		return s, id
	}
	placed := func(t *testing.T, s *Server, id string) (string, spec.Resources) {
		t.Helper()
		var host string
		var res spec.Resources
		systemScan(t, s, `SELECT coalesce(p.host_id, ''), coalesce(p.resources, '{}') FROM runs r
			LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch AND p.epoch > 1 WHERE r.id = $1`, []any{id}, &host, &res)
		return host, res
	}

	t.Run("smaller, on its snapshot's host", func(t *testing.T) {
		s, id := setup(t)
		readyHost(t, s, "hb", "default", "", true)
		execSQL(t, s, context.Background(), `UPDATE hosts SET capacity = '{"cpus":64,"memory":274877906944}' WHERE id = 'hb'`)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, res := placed(t, s, id)
		if host != "ha" || res.CPUs != 1 || int64(res.Memory) != gib {
			t.Fatalf("placed on %q with %+v, want ha with 1 cpu, 1 GiB", host, res)
		}
	})
	t.Run("larger than its snapshot's host, elsewhere", func(t *testing.T) {
		s, id := setup(t)
		readyHost(t, s, "hb", "default", "", false)
		execSQL(t, s, context.Background(), `UPDATE hosts SET capacity = '{"cpus":16,"memory":68719476736}' WHERE id = 'hb'`)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(8), Memory: bytesPtr(32 * gib)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, res := placed(t, s, id)
		if host != "hb" || res.CPUs != 8 || int64(res.Memory) != 32*gib {
			t.Fatalf("placed on %q with %+v, want hb with 8 cpus, 32 GiB", host, res)
		}
	})
	t.Run("larger than any host, waits as a submit would", func(t *testing.T) {
		s, id := setup(t)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(8)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, _ := placed(t, s, id)
		_, _, reason := placedOn(t, s, id)
		const want = "waiting for capacity: 1 host in its pool lacks cpus (requested 8)"
		if host != "" || reason != want {
			t.Fatalf("host %q reason %q, want none and %q", host, reason, want)
		}
		other := submitAs(t, s, "t1", "default", "")
		execSQL(t, s, context.Background(), `UPDATE runs SET spec = jsonb_set(spec, '{resources,cpus}', '8') WHERE id = $1`, other)
		schedule(t, s)
		if _, _, r := placedOn(t, s, other); r != want {
			t.Fatalf("a submit of 8 cpus waits with %q, want %q", r, want)
		}
	})
}

// stoppingRun is a Run of tenant t1 (20 GiB disk) whose placement 1 on
// host ha is stopping, as stop leaves it: the Run is running, the
// placement asked to stop. Returns its id.
func stoppingRun(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	namedPools(t, s, "default")
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Volumes:   []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}},
		Resources: spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)},
		Placement: spec.Placement{Pool: "default"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', 'default', 'ready', '{"cpus":4,"memory":8589934592}', now()) ON CONFLICT DO NOTHING`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, stop_reason, lease_expires_at)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'stopping', '{}', 'stop', now() + interval '1 minute')`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping', current_epoch = 1, pool_id = 'default' WHERE id = $1`, id)
	return id
}

// A placement's peak disk is only a measurement of its snapshot once its
// exit status (which carries the final sample, taken after the snapshot
// was reported) has arrived. Lost between the two, a heartbeat's older,
// smaller peak does not admit a shrink: the disk is kept, saying why, and
// the Run resumes with its cpus and memory changed.
func TestResumeDiskShrinkNeedsTheFinalMeasurement(t *testing.T) {
	// The exit status cases stay under 2 GiB: recordUsage cannot store a
	// larger value (its nullif parameters are typed int4).
	const mib = 1 << 20
	for _, c := range []struct {
		name    string
		saved   int64 // the snapshot's final disk sample
		final   bool  // the exit status, with that sample, arrived
		disk    int64
		applied int64
		reason  string
	}{
		{"lost before its exit status", 8 * gib, false, 2 * gib, 20 * gib, "no final measurement"},
		{"lost before its exit status, a shrink its peak admits", 8 * gib, false, 11 * gib, 20 * gib, "no final measurement"},
		{"exited, its final usage refuses the shrink", 1800 * mib, true, 2 * gib, 20 * gib, "its saved state used up to"},
		{"exited, its final usage admits the shrink", 1800 * mib, true, 3 * gib, 3 * gib, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			id := stoppingRun(t, s)
			if err := s.heartbeat(ctx, "ha", proto.Heartbeat{Leases: []proto.LivePlacement{
				{RunID: id, Epoch: 1, State: "running", Usage: &proto.Usage{PeakDiskBytes: 100 * mib}}}}); err != nil {
				t.Fatal(err)
			}
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				_, err := s.applySnapshotDone(ctx, tx, "t1", "ha", id, 1, 1, proto.SnapshotDone{Manifest: proto.Manifest{
					SnapshotID: "snap-" + id, RunID: id, Epoch: 1,
					Volumes: []proto.VolumeSnapshot{{Name: "data", Path: "/data", BlobID: "blob-" + id, Size: c.saved, SHA256: "x"}}}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if c.final {
					code := 0
					return s.applyStatus(ctx, tx, "t1", id, 1, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped",
						Usage: &proto.Usage{PeakDiskBytes: c.saved}})
				}
				var later laterEvents
				if err := s.placementLost(ctx, tx, id, 1, "host lost: missed heartbeats", &later); err != nil {
					return err
				}
				return later.write()
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(c.disk)})
			if err != nil {
				t.Fatal(err)
			}
			want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(c.applied), Pids: 1024}
			if got := storedResources(t, s, id); got != want || out.Body.State != StateResuming {
				t.Errorf("%s with %+v, want resuming with %+v", out.Body.State, got, want)
			}
			rz := out.Body.Resize
			if rz == nil || rz.Applied != want || (rz.Disk != nil) != (c.reason != "") {
				t.Fatalf("answer's resize %+v", rz)
			}
			if c.reason != "" && !strings.Contains(rz.Disk.Reason, c.reason) {
				t.Errorf("reason %q, want it to say %q", rz.Disk.Reason, c.reason)
			}
		})
	}
}

// A Run already resuming takes a retry of its resume's request (the same
// requested resources, or none) and answers it with that resume's resize;
// any other resources are refused, 409, invalid ones 422. Nothing changes.
func TestResumeRetryWhileResuming(t *testing.T) {
	first := &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(3*gib - 1)} // under the 3 GiB floor: kept
	for _, c := range []struct {
		name   string
		retry  *resumeResources
		status int // 0: 202
		resize bool
	}{
		{"repeat of the refused shrink", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(3*gib - 1)}, 0, true},
		{"no resources", nil, 0, false},
		{"empty resources", &resumeResources{}, 0, false},
		{"the applied disk", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(20 * gib)}, http.StatusConflict, false},
		{"a new feasible shrink", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(10 * gib)}, http.StatusConflict, false},
		{"a new impossible shrink", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(gib)}, http.StatusConflict, false},
		{"the disk alone", &resumeResources{Disk: spec.Bytes(3*gib - 1)}, http.StatusConflict, false},
		{"other cpus", &resumeResources{CPUs: f64(2), Memory: bytesPtr(gib), Disk: spec.Bytes(3*gib - 1)}, http.StatusConflict, false},
		{"negative disk", &resumeResources{Disk: -1}, http.StatusUnprocessableEntity, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, i64(2*gib))
			out1, err := resumeWith(s, id, first)
			if err != nil {
				t.Fatal(err)
			}
			if out1.Body.Resize == nil || out1.Body.Resize.Disk == nil {
				t.Fatalf("first resume's resize %+v, want the disk kept", out1.Body.Resize)
			}
			out2, err := resumeWith(s, id, c.retry)
			if c.status != 0 {
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != c.status {
					t.Fatalf("retry: %v, want %d", err, c.status)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got := out2.Body.Resize
				if c.resize && (got == nil || !reflect.DeepEqual(*got, *out1.Body.Resize)) {
					t.Errorf("retry's resize %+v, want the first's %+v", got, out1.Body.Resize)
				}
				if !c.resize && got != nil {
					t.Errorf("retry's resize %+v, want none", got)
				}
			}
			want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(20 * gib), Pids: 1024}
			var events int
			systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'resume.requested'`, []any{id}, &events)
			if got := storedResources(t, s, id); got != want || events != 1 {
				t.Errorf("after the retry: %+v and %d resume.requested, want %+v and 1", got, events, want)
			}
		})
	}
}

// A Run resumed again after a resume that resized it, which ran and
// stopped, is compared with the new resume, not the old one.
func TestResumeRetryComparesTheCurrentResume(t *testing.T) {
	s := testServer(t)
	id := stoppedRun(t, s, spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}, i64(gib))
	if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(1)}); err != nil {
		t.Fatal(err)
	}
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return setRunState(context.Background(), tx, "t1", id, StateStopped, "stop", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeWith(s, id, nil); err != nil {
		t.Fatal(err)
	}
	_, err = resumeWith(s, id, &resumeResources{CPUs: f64(1)})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("retry with the earlier resume's cpus: %v, want 409", err)
	}
	if out, err := resumeWith(s, id, nil); err != nil || out.Body.Resize != nil {
		t.Fatalf("retry without resources: %v, resize %+v", err, out)
	}
}
