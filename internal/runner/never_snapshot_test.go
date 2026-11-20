package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// A placement's exit snapshot by resumePolicy: a never Run, which nothing
// resumes, exports no state volume (none in its manifest, none uploaded,
// the stale local copy it held no longer claimed) but still reports its
// snapshot with its output, artifacts and output seq, then its end; every
// other policy exports its state volume as before.
func TestExitSnapshotExportsStateVolumesUnlessNever(t *testing.T) {
	for _, c := range []struct {
		policy  string
		volumes bool
	}{
		{"", true}, {"auto", true}, {"restart", true}, {"manual", true}, {"never", false},
	} {
		t.Run(cmp.Or(c.policy, "unset"), func(t *testing.T) {
			f := newFinishFixture(t)
			f.serveUploads(t)
			f.p.assign.Spec.ResumePolicy = c.policy
			f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
			f.p.state.VolumesSnapshot, f.p.state.VolumesEpoch = "snap_prev", 0
			writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
			f.release()
			f.p.setPhase("exited")
			go func() {
				defer close(f.p.done)
				f.p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped", OutputSeq: 7})
			}()
			mustEnd(t, f.p, "the placement never ended")

			if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
				t.Fatalf("reports %v, want snapshot.done then status", got)
			}
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" || sd.Output == nil {
				t.Fatalf("snapshot.done %+v: want a snapshot id and the output, no error", sd)
			}
			if sd.OutputSeq != 7 {
				t.Errorf("snapshot.done output seq %d, want 7", sd.OutputSeq)
			}
			if len(sd.Artifacts) != 1 || sd.Artifacts[0].Path != "/data/state" {
				t.Fatalf("snapshot.done artifacts %+v, want /data/state", sd.Artifacts)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes {
				t.Errorf("volume export run: %v, want %v; podman:\n%s", exported, c.volumes, f.podmanLog())
			}
			want := []string{sd.Output.BlobID, sd.Artifacts[0].BlobID}
			if c.volumes {
				if len(sd.Manifest.Volumes) != 1 || sd.Manifest.Volumes[0].Name != "data" {
					t.Fatalf("manifest volumes %+v, want data", sd.Manifest.Volumes)
				}
				want = append(want, sd.Manifest.Volumes[0].BlobID)
			} else if len(sd.Manifest.Volumes) != 0 {
				t.Fatalf("manifest volumes %+v, want none", sd.Manifest.Volumes)
			}

			f.r.uploads.pass(context.Background())
			f.mu.Lock()
			uploaded := slices.Clone(f.uploaded)
			f.mu.Unlock()
			slices.Sort(uploaded)
			slices.Sort(want)
			if !slices.Equal(uploaded, want) {
				t.Errorf("uploaded %v, want %v", uploaded, want)
			}

			st, err := readRunState(f.p.dir)
			if err != nil || st.Phase != "reported" {
				t.Fatalf("run state %+v (%v), want reported", st, err)
			}
			wantLocal := ""
			if c.volumes {
				wantLocal = sd.Manifest.SnapshotID
			}
			if st.VolumesSnapshot != wantLocal {
				t.Errorf("local volumes hold snapshot %q, want %q", st.VolumesSnapshot, wantLocal)
			}
			held := slices.ContainsFunc(f.r.localSnapshots(), func(l proto.LocalSnapshot) bool { return l.RunID == "run1" })
			if held != c.volumes {
				t.Errorf("heartbeat lists a local copy: %v, want %v", held, c.volumes)
			}
		})
	}
}

// A transient placement exports its state volume only when stopped for a
// move (drain, preempt, migrate), to be resumed elsewhere; any other end
// (success, failure, idle, a requested stop, a timeout) exports none, as
// for never, and still reports its output and artifacts. A terminate that
// overrides a move's stop exports nothing either.
func TestTransientExitSnapshotExportsOnlyOnAMove(t *testing.T) {
	for _, c := range []struct {
		stops   []string
		code    int
		volumes bool
	}{
		{nil, 0, false}, {nil, 1, false}, {[]string{"stop"}, 0, false}, {[]string{"idle"}, 0, false},
		{[]string{"timeout"}, 137, false}, {[]string{"preempt", "terminate"}, 0, false},
		{[]string{"drain"}, 0, true}, {[]string{"preempt"}, 0, true}, {[]string{"migrate"}, 0, true},
	} {
		t.Run(fmt.Sprintf("%v exit %d", c.stops, c.code), func(t *testing.T) {
			f := newFinishFixture(t)
			f.serveUploads(t)
			f.p.assign.Spec.ResumePolicy = spec.ResumeTransient
			f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
			writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
			for _, why := range c.stops {
				f.p.mu.Lock()
				if f.p.stopWhy == "" || terminateStop(why) {
					f.p.stopWhy = why
				}
				f.p.mu.Unlock()
			}
			f.release()
			f.p.setPhase("exited")
			go func() {
				defer close(f.p.done)
				f.p.finish(context.Background(), &exitRecord{Code: c.code, Reason: "stopped", OutputSeq: 3})
			}()
			mustEnd(t, f.p, "the placement never ended")
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" || sd.Output == nil || len(sd.Artifacts) != 1 {
				t.Fatalf("snapshot.done %+v: want a snapshot id, the output and the artifact", sd)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes || (len(sd.Manifest.Volumes) == 1) != c.volumes || (!c.volumes && len(sd.Manifest.Volumes) != 0) {
				t.Fatalf("volume export run %v, manifest volumes %+v: want exported %v", exported, sd.Manifest.Volumes, c.volumes)
			}
			wantMove := ""
			if c.volumes {
				wantMove = c.stops[0]
			}
			if sd.Manifest.MoveStop != wantMove {
				t.Fatalf("manifest moveStop %q, want %q", sd.Manifest.MoveStop, wantMove)
			}
			if rec := f.r.snapshotRecords()[sd.Manifest.SnapshotID]; rec == nil || rec.MoveStop != wantMove {
				t.Fatalf("saved record %+v, want moveStop %q", rec, wantMove)
			}
		})
	}
}

// The runner records a move in the manifest even when the spec declares no
// state volume: provenance is not inferred from the volume count.
func TestTransientMoveWithoutStateVolumesRecordsMove(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	f.p.assign.Spec.ResumePolicy = spec.ResumeTransient
	f.p.state.Volumes = nil
	f.p.stopWhy = "migrate"
	f.release()
	f.p.setPhase("exited")
	go func() {
		defer close(f.p.done)
		f.p.finish(context.Background(), &exitRecord{Code: 143, Reason: "stopped"})
	}()
	mustEnd(t, f.p, "the placement never ended")
	sd := f.snapshotDone(t)
	if len(sd.Manifest.Volumes) != 0 || sd.Manifest.MoveStop != "migrate" {
		t.Fatalf("manifest %+v, want zero volumes and moveStop migrate", sd.Manifest)
	}
}

// A transient natural-exit snapshot remains a natural exit even if luxd's
// move request reaches the runner afterwards: once uploaded, its copy can be
// reclaimed. An upload still pending keeps it.
func TestTransientDiskGuardUsesSnapshotDecision(t *testing.T) {
	for _, c := range []struct {
		name     string
		move     string
		uploaded bool
		want     bool
	}{
		{"natural uploaded", "", true, true},
		{"natural uploading", "", false, false},
		{"move uploaded", "drain", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDiskFixture(t)
			st := &runState{RunID: "run1", Epoch: 1, Phase: "reported", LastExitAt: 1, StopReason: "drain",
				Spec: &spec.RunSpec{ResumePolicy: spec.ResumeTransient}}
			if err := writeRunState(f.r.runDir("run1"), st); err != nil {
				t.Fatal(err)
			}
			saveRecord(t, f.r, "s1", &snapshotRecord{RunID: "run1", Epoch: 1, Reported: true, MoveStop: c.move,
				Uploads: []pendingUpload{{BlobID: "out", Size: 1, Done: c.uploaded}}})
			if got := f.r.localCopies(); len(got) != 1 || got[0].reclaimable != c.want {
				t.Fatalf("copies %+v, want reclaimable %v", got, c.want)
			}
		})
	}
}

// A placement re-adopted after a restart, its container exited, takes its
// resumePolicy from the spec its run state stored: a never Run exports no
// state volume. A run state with no spec (no assignment to read a policy
// from) exports them, as for a Run that may be resumed.
func TestReadoptedExitSnapshotExportsStateVolumesUnlessNever(t *testing.T) {
	for _, c := range []struct {
		name    string
		spec    *spec.RunSpec
		volumes bool
	}{
		{"never", &spec.RunSpec{ResumePolicy: "never"}, false},
		{"no spec", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			f.release()
			f.p.state.Phase = "exited"
			f.p.state.Exit = &exitRecord{Code: 0, Reason: "stopped"}
			f.p.state.Spec = c.spec
			if err := writeRunState(f.p.dir, f.p.state); err != nil {
				t.Fatal(err)
			}
			f.r.mu.Lock()
			f.r.placements = map[string]*placement{}
			f.r.mu.Unlock()
			f.r.readopt(context.Background())
			f.r.mu.Lock()
			p := f.r.placements["run1"]
			f.r.mu.Unlock()
			if p == nil {
				t.Fatal("the finishing placement was not re-adopted")
			}
			if (p.assign == nil) != (c.spec == nil) {
				t.Fatalf("re-adopted assignment %+v, want one only from a stored spec", p.assign)
			}
			mustEnd(t, p, "the re-adopted placement never ended")
			if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
				t.Fatalf("reports %v, want snapshot.done then status", got)
			}
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" {
				t.Fatalf("snapshot.done %+v: want a snapshot id, no error", sd)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes || (len(sd.Manifest.Volumes) == 1) != c.volumes {
				t.Errorf("volume export run %v, manifest volumes %+v: want exported %v; podman:\n%s",
					exported, sd.Manifest.Volumes, c.volumes, f.podmanLog())
			}
		})
	}
}

// snapshotDone is the first snapshot.done luxd got.
func (f *finishFixture) snapshotDone(t *testing.T) proto.SnapshotDone {
	t.Helper()
	f.mu.Lock()
	i := slices.IndexFunc(f.reports, func(fr proto.Frame) bool { return fr.Type == proto.MsgSnapshotDone })
	var data json.RawMessage
	if i >= 0 {
		data = f.reports[i].Data
	}
	f.mu.Unlock()
	var sd proto.SnapshotDone
	if err := json.Unmarshal(data, &sd); err != nil {
		t.Fatalf("snapshot.done %s: %v", data, err)
	}
	return sd
}
