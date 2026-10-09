package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanForAStart is a fake podman that takes a placement through its
// whole start: a network, an image the host has (mounted at $0.root), a
// container. `start` waits while $0.holdstart exists; the container is
// then inspected as exited, so the shim is never reached, and `wait`
// blocks until the test ends.
const podmanForAStart = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"network create") ;;
"network inspect") echo "lux0 10.88.0.1" ;;
"image exists") ;;
"image inspect") echo '[{"Id":"img1","Config":{"User":""}}]' ;;
"image mount") echo "$0.root" ;;
"image unmount") ;;
"volume exists") exit 1 ;;
"volume create") ;;
"container inspect")
  [ -e "$0.ctr" ] || { echo "no such container" >&2; exit 125; }
  echo '[{"State":{"Status":"exited","Running":false}}]' ;;
"rm -f") ;;
"kill -s") ;;
*)
  case "$1" in
  create) echo ctr-1 > "$0.ctr"; echo ctr-1 ;;
  start) while [ -e "$0.holdstart" ]; do sleep 0.02; done ;;
  wait) while :; do sleep 0.05; done ;;
  *) exit 1 ;;
  esac ;;
esac
`

// Each start phase reaches luxd as a starting status while the start is
// still under way, with every mark so far: the image, volumes and
// repositories before the container starts, the container's start before
// the placement runs or ends. Reports arrive in order: each carries the
// marks of the one before it.
func TestStartPhasesAreReportedAsTheyHappen(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanForAStart), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"rt", "podman.root", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin+".holdstart", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dir, Shim: "/bin/true"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded(),
		images: newImageUse(dir)}
	r.conn = newConn(r)
	r.conn.polling = true
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))

	var mu sync.Mutex
	var reports []proto.Status // every status report, in the order luxd got them
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			frames := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, fr := range frames {
				var st proto.Status
				if fr.Type == proto.MsgStatus && json.Unmarshal(fr.Data, &st) == nil {
					mu.Lock()
					reports = append(reports, st)
					mu.Unlock()
				}
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
			}
			time.Sleep(time.Millisecond)
		}
	})

	sp := spec.RunSpec{Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true}}
	p := newPlacement(r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1, Spec: sp}, nil)
	r.placements["run1"] = p
	go p.run(ctx)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		select {
		case <-p.done:
		case <-time.After(45 * time.Second):
			t.Error("the placement did not end")
		}
	})

	reported := func() []proto.Status {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reports)
	}
	waitMark := func(mark string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			for _, st := range reported() {
				if st.State == "starting" && st.Times[mark] != 0 {
					return
				}
			}
			if time.Now().After(deadline) {
				log, _ := os.ReadFile(bin + ".log")
				t.Fatalf("no starting report with %s; reports %+v; podman:\n%s", mark, reported(), log)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The container's start is held: everything before it is reported.
	waitMark("reposReady")
	for _, st := range reported() {
		if st.State != "starting" || st.Times["containerStarted"] != 0 {
			t.Fatalf("while the container's start is held: %+v", st)
		}
	}
	last := reported()
	got := last[len(last)-1].Times
	if got["imageReady"] == 0 || got["volumesRestored"] == 0 ||
		got["imageReady"] > got["volumesRestored"] || got["volumesRestored"] > got["reposReady"] {
		t.Fatalf("marks %v: want imageReady <= volumesRestored <= reposReady", got)
	}

	if err := os.Remove(bin + ".holdstart"); err != nil {
		t.Fatal(err)
	}
	// Started: reported while the container has not exited (wait blocks).
	waitMark("containerStarted")
	all := reported()
	for i, st := range all {
		if st.State != "starting" {
			t.Fatalf("report %d: %+v; want only starting reports before the container exits", i, st)
		}
		if i == 0 {
			continue
		}
		for k, v := range all[i-1].Times {
			if st.Times[k] != v {
				t.Fatalf("report %d %v drops or changes %s of report %d %v", i, st.Times, k, i-1, all[i-1].Times)
			}
		}
	}
	if len(all) < 2 {
		t.Fatalf("reports %+v: want the phases reported apart", all)
	}
}
