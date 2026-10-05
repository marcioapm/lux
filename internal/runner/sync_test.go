package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
