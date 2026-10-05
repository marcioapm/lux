package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Two syncs of one Run under way at once keep their own bundles: preparing
// the second leaves the first's for its shim to fetch, and each is removed
// when its own sync is done.
func TestConcurrentSyncsKeepTheirBundles(t *testing.T) {
	f := newSyncFixture(t, `true`)
	p, sp := f.p, f.sp
	rt := filepath.Join(p.r.cfg.DataDir, "rt")
	refs := []proto.SyncRef{{Repo: "app", Ref: "main"}}
	ctx := context.Background()

	first, _ := p.prepareSync(ctx, sp, refs, "s1", false)
	second, _ := p.prepareSync(ctx, sp, refs, "s2", false)
	if first == nil || second == nil || first.Repos[0].Bundle == second.Repos[0].Bundle {
		t.Fatalf("bundles: %+v %+v", first, second)
	}
	host := func(b string) string { return filepath.Join(rt, strings.TrimPrefix(b, proto.ShimRunDir+"/")) }
	for _, a := range []*proto.SyncArgs{first, second} {
		if _, err := os.Stat(host(a.Repos[0].Bundle)); err != nil {
			t.Fatalf("a sync's bundle is gone before its shim ran: %v", err)
		}
	}
	p.removeSyncBundles(ctx, "s1")
	if _, err := os.Stat(host(first.Repos[0].Bundle)); !os.IsNotExist(err) {
		t.Fatalf("s1's bundle after s1: %v", err)
	}
	if _, err := os.Stat(host(second.Repos[0].Bundle)); err != nil {
		t.Fatalf("s2's bundle after s1: %v", err)
	}
	// A request id is never a path.
	if d := syncSubdir("../../etc"); strings.ContainsAny(d, "./") {
		t.Fatalf("subdir of a hostile id: %q", d)
	}
}

// A sync's mode reaches lux-shim sync, in the first try and in the
// whole-history retry; a sync whose checkouts were kept, ahead or only
// fetched moved nothing: sync.done says changed false, and the next
// bundle's base stays where it was.
func TestSyncModeReachesTheShim(t *testing.T) {
	for _, c := range []struct {
		status  string
		changed bool
	}{{"kept", false}, {"ahead", false}, {"fetched", false}, {"fast-forward", true}} {
		t.Run(c.status, func(t *testing.T) {
			// Each lux-shim sync's argument (podman's last) to $0.args.<n>;
			// the first answers missingBase, the retry c.status.
			f := newSyncFixture(t, `for a; do last=$a; done
n=$(cat "$0.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0.n"
printf '%s' "$last" > "$0.args.$n"
if [ $n = 1 ]; then
  echo '[{"repo":"app","ref":"main","status":"failed","error":"no base","missingBase":true}]'
else
  echo '[{"repo":"app","ref":"main","mode":"fast-forward","status":"`+c.status+`","to":"0123456789012345678901234567890123456789"}]'
fi`)
			f.p.state.GitBases = map[string]string{"app": "base"}
			f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq1", Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFastForward}}})
			bin := f.p.r.pm.Bin
			for _, n := range []string{"1", "2"} {
				b, err := os.ReadFile(bin + ".args." + n)
				if err != nil {
					t.Fatalf("lux-shim sync %s: %v", n, err)
				}
				var a proto.SyncArgs
				if err := json.Unmarshal(b, &a); err != nil || len(a.Repos) != 1 || a.Repos[0].Mode != proto.SyncFastForward {
					t.Fatalf("lux-shim sync %s's args: %s", n, b)
				}
			}
			waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.done) == 1 })
			if evs := f.events(); len(evs) != 1 || evs[0].Status != c.status || !evs[0].FullBundle {
				t.Fatalf("git.sync: %+v", evs)
			}
			if got := f.done[0]["changed"]; got != c.changed {
				t.Fatalf("sync.done changed %v, want %v", got, c.changed)
			}
			f.p.mu.Lock()
			base := f.p.state.GitBases["app"]
			f.p.mu.Unlock()
			if moved := base != "base"; moved != c.changed {
				t.Fatalf("base %q after %s", base, c.status)
			}
		})
	}
}

// A placement whose container started with a lux-shim older than sync
// modes never runs a fast-forward or fetch there (that shim would move):
// each fails, naming why, without lux-shim sync. Mode move still runs.
func TestSyncModesOnAnOldShim(t *testing.T) {
	f := newSyncFixture(t, `echo called >> "$0.calls"
echo '[{"repo":"app","ref":"main","status":"up-to-date"}]'`)
	f.p.state.ShimSyncModes = false
	for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
		f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq-" + mode, Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: mode}}})
	}
	if _, err := os.Stat(f.p.r.pm.Bin + ".calls"); !os.IsNotExist(err) {
		t.Fatalf("lux-shim sync ran on an old shim: %v", err)
	}
	waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.done) == 2 })
	evs := f.events()
	if len(evs) != 2 {
		t.Fatalf("git.sync: %+v", evs)
	}
	for _, ev := range evs {
		if ev.Status != "failed" || ev.Error != "this Run's lux-shim predates sync modes; resume it to update" {
			t.Fatalf("git.sync: %+v", ev)
		}
	}
	for _, mode := range []string{"", proto.SyncMove} {
		f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq-move" + mode, Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: mode}}})
	}
	b, _ := os.ReadFile(f.p.r.pm.Bin + ".calls")
	if strings.Count(string(b), "called") != 2 {
		t.Fatalf("mode move on an old shim: %d lux-shim syncs, want 2", strings.Count(string(b), "called"))
	}
}

// The runner asks its --shim whether it knows sync modes, and says so in
// Hello: the shim built from this tree does; one that ignores the mode
// (an older shim, which reports none) does not.
func TestShimKnowsSyncModes(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "lux-shim")
	build := exec.Command("go", "build", "-o", shim, "github.com/marcioapm/lux/cmd/lux-shim")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if !shimKnowsSyncModes(shim) {
		t.Fatal("this tree's lux-shim: no sync modes")
	}
	old := filepath.Join(dir, "old-shim")
	os.WriteFile(old, []byte("#!/bin/sh\necho '[{\"repo\":\"probe\",\"ref\":\"\",\"status\":\"failed\",\"error\":\"/nonexistent has no checkout\"}]'\n"), 0o755)
	if shimKnowsSyncModes(old) || shimKnowsSyncModes(filepath.Join(dir, "missing")) {
		t.Fatal("an old or missing lux-shim: sync modes")
	}
	r := &Runner{shimSyncModes: false}
	if slices.Contains(r.capabilities(), proto.CapSyncModes) {
		t.Fatal("Hello offers sync modes with an old shim")
	}
	r.shimSyncModes = true
	if !slices.Contains(r.capabilities(), proto.CapSyncModes) {
		t.Fatal("Hello without sync modes")
	}
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
