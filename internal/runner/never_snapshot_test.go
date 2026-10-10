package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// A placement's exit snapshot by resumePolicy: a never Run, which nothing
// resumes, exports no state volume (none in its manifest, none uploaded,
// no local copy claimed) but still reports its snapshot with its output,
// then its end; every other policy exports its state volume as before.
func TestExitSnapshotExportsStateVolumesUnlessNever(t *testing.T) {
	for _, c := range []struct {
		policy  string
		volumes bool
	}{
		{"", true}, {"auto", true}, {"restart", true}, {"manual", true}, {"never", false},
	} {
		t.Run(cmpName(c.policy), func(t *testing.T) {
			f := newFinishFixture(t)
			f.serveUploads(t)
			f.p.assign.Spec.ResumePolicy = c.policy
			writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
			f.release()
			f.exitedAsSupervised(context.Background())
			mustEnd(t, f.p, "the placement never ended")

			if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
				t.Fatalf("reports %v, want snapshot.done then status", got)
			}
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" || sd.Output == nil {
				t.Fatalf("snapshot.done %+v: want a snapshot id and the output, no error", sd)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes {
				t.Errorf("volume export run: %v, want %v; podman:\n%s", exported, c.volumes, f.podmanLog())
			}
			want := []string{sd.Output.BlobID}
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

func cmpName(policy string) string {
	if policy == "" {
		return "unset"
	}
	return policy
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
