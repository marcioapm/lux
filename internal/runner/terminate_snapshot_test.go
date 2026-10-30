package runner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// A placement's exit snapshot by the stop that ended it: a terminate (or
// "cancel", an older luxd's terminate, or a terminate overriding an earlier
// stop) exports no state volume, whatever the resumePolicy, but still
// reports and uploads its output and artifacts; every other stop exports
// the state volume as before.
func TestExitSnapshotExportsStateVolumesUnlessTerminated(t *testing.T) {
	for _, c := range []struct {
		name    string
		stops   []string
		volumes bool
	}{
		{"terminate", []string{"terminate"}, false},
		{"cancel", []string{"cancel"}, false},
		{"stop then terminate", []string{"stop", "terminate"}, false},
		{"idle then terminate", []string{"idle", "terminate"}, false},
		{"stop", []string{"stop"}, true},
		{"idle", []string{"idle"}, true},
		{"disk-pressure", []string{"disk-pressure"}, true},
		{"migrate", []string{"migrate"}, true},
		{"drain", []string{"drain"}, true},
		{"timeout", []string{"timeout"}, true},
		{"preempt", []string{"preempt"}, true},
		{"disk", []string{"disk"}, true},
		{"preempt then terminate", []string{"preempt", "terminate"}, false},
		{"spot-reclaim", nil, true},
		{"spot-reclaim then terminate", []string{"terminate"}, true},
		{"no stop", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			f.serveUploads(t)
			f.p.assign.Spec.ResumePolicy = "auto"
			f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
			f.p.state.VolumesSnapshot, f.p.state.VolumesEpoch = "snap_prev", 0
			if strings.HasPrefix(c.name, "spot-reclaim") {
				// requestStop would arm the reclaim kill; only the reason matters here.
				f.p.stopWhy = stopSpotReclaim
			}
			for _, why := range c.stops {
				f.p.requestStop(context.Background(), why)
			}
			writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
			f.release()
			f.p.setPhase("exited")
			go func() {
				defer close(f.p.done)
				f.p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped", OutputSeq: 7})
			}()
			mustEnd(t, f.p, "the placement never ended")

			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" || sd.Output == nil {
				t.Fatalf("snapshot.done %+v: want a snapshot id and the output, no error", sd)
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

// A placement re-adopted after a restart, its container exited, takes its
// stop reason from its run state: a persisted terminate exports no state
// volume, a persisted stop or spot reclaim exports it.
func TestReadoptedExitSnapshotExportsStateVolumesUnlessTerminated(t *testing.T) {
	for _, c := range []struct {
		reason  string
		volumes bool
	}{
		{"terminate", false},
		{"cancel", false},
		{"stop", true},
		{"idle", true},
		{"preempt", true},
		{stopSpotReclaim, true},
	} {
		t.Run(c.reason, func(t *testing.T) {
			f := newFinishFixture(t)
			f.release()
			f.p.state.Phase = "exited"
			f.p.state.Exit = &exitRecord{Code: 0, Reason: "stopped"}
			f.p.state.Spec = &spec.RunSpec{ResumePolicy: "auto"}
			f.p.state.StopReason = c.reason
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
			mustEnd(t, p, "the re-adopted placement never ended")
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

// The export decision is taken when the snapshot starts: a terminate that
// arrives while a state volume is being exported does not abandon it, and
// the snapshot reports the volume.
func TestTerminateDuringExportKeepsTheExport(t *testing.T) {
	f := newFinishFixture(t)
	f.p.requestStop(context.Background(), "stop")
	f.p.setPhase("exited")
	go func() {
		defer close(f.p.done)
		f.p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	}()
	f.waitFile(t, f.bin+".exporting")
	f.p.requestStop(context.Background(), "terminate")
	f.release()
	mustEnd(t, f.p, "the placement never ended")
	if sd := f.snapshotDone(t); sd.Error != "" || len(sd.Manifest.Volumes) != 1 {
		t.Fatalf("snapshot.done %+v: want the data volume, no error", sd)
	}
	if st, err := readRunState(f.p.dir); err != nil || st.StopReason != "terminate" {
		t.Fatalf("run state %+v (%v), want stop reason terminate", st, err)
	}
}
