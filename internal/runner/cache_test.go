package runner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func TestCacheTenantIsolationAndResume(t *testing.T) {
	f := newFinishFixture(t)
	// Fake Podman stores real files for each named volume.
	script := `#!/bin/sh
case "$1 $2" in
"volume exists") test -d "$0.volumes/$3" ;;
"volume create") for n; do :; done; mkdir -p "$0.volumes/$n" ;;
"volume inspect") echo "$0.volumes/$5" ;;
"volume rm") rm -r "$0.volumes/$3" ;;
*) exit 1 ;;
esac
`
	writeFile(t, f.bin, script)
	ctx := context.Background()
	prepare := func(run, tenant string, uid, gid int, name string) *placement {
		p := &placement{r: f.r, runID: run, tenantID: tenant, epoch: 1, dir: f.r.runDir(run), state: &runState{RunID: run}, user: passwd.User{UID: uid, GID: gid}}
		sp := spec.RunSpec{Volumes: []spec.Volume{{Name: name, Path: "/cache", Kind: "cache"}}}
		if err := p.prepareVolumes(ctx, sp, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := prepare("a", "tenant-a", 1000, 1000, "gomod")
	mp := filepath.Join(f.bin+".volumes", a.state.Volumes[0].Volume)
	writeFile(t, filepath.Join(mp, "module"), "cached")
	for _, c := range []struct {
		run, tenant, name string
		uid, gid          int
		want              bool
	}{
		{"b", "tenant-a", "gomod", 1000, 1000, true},
		{"a", "tenant-a", "gomod", 1000, 1000, true},
		{"c", "tenant-b", "gomod", 1000, 1000, false},
		{"d", "tenant-a", "gomod", 1001, 1000, false},
		{"e", "tenant-a", "gomod", 1000, 1001, false},
		{"f", "tenant-a", "npm", 1000, 1000, false},
		{"g", "tenant-a", "gomod", 1000, 1000, true},
	} {
		p := prepare(c.run, c.tenant, c.uid, c.gid, c.name)
		b, err := os.ReadFile(filepath.Join(f.bin+".volumes", p.state.Volumes[0].Volume, "module"))
		if c.want && (err != nil || string(b) != "cached") || !c.want && !os.IsNotExist(err) {
			t.Fatalf("%+v: %q %v", c, b, err)
		}
	}
}

func TestCacheFreeHoldDoesNotWaitForLifecycle(t *testing.T) {
	f := newFinishFixture(t)
	u := f.r.cacheUse()
	u.mu.Lock()
	defer u.mu.Unlock()
	done := make(chan struct{})
	go func() {
		f.p.holdCaches(spec.RunSpec{})()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cache-free hold/release blocked on cache lifecycle")
	}
	// A fresh runner needs no cache bookkeeping at all for a cache-free Run.
	(&placement{}).holdCaches(spec.RunSpec{})()
}

func TestCacheMetadataPreparationAndRelease(t *testing.T) {
	for _, metadata := range []string{"missing", "null", "{", "{}"} {
		t.Run(metadata, func(t *testing.T) {
			f := newFinishFixture(t)
			writeFile(t, f.bin, `#!/bin/sh
case "$1 $2" in
"volume exists") exit 1 ;;
"volume create"|"volume ls") ;;
*) exit 1 ;;
esac
`)
			if metadata != "missing" {
				writeFile(t, filepath.Join(f.r.cfg.DataDir, "caches.json"), metadata)
			}
			sp := spec.RunSpec{Volumes: []spec.Volume{{Name: "gomod", Path: "/cache", Kind: "cache"}}}
			release := f.p.holdCaches(sp)
			if err := f.p.prepareVolumes(context.Background(), sp, nil); err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestCacheDiscoveryReclaimsLostMetadata(t *testing.T) {
	for _, metadata := range []string{"missing", "{", "null", `{"recent":100,"old":1}`} {
		t.Run(metadata, func(t *testing.T) {
			f := newFinishFixture(t)
			writeFile(t, f.bin, `#!/bin/sh
case "$1 $2" in
"volume ls")
  case "$4" in
  label=lux.managed=true) printf 'unknown\nattached\nrecent\nold\nnot-cache\n' ;;
  label=lux.cache=true) printf 'unknown\nattached\nrecent\nold\nunmanaged\n' ;;
  *) exit 1 ;;
  esac ;;
"volume rm")
  shift 2
  force=false
  n=
  for arg; do
    case "$arg" in -f|--force) force=true ;; *) n="$arg" ;; esac
  done
  [ "$n" = attached ] && [ "$force" = false ] && exit 1
  rm "$0.cache.$n" ;;
*) exit 1 ;;
esac
`)
			if metadata != "missing" {
				writeFile(t, filepath.Join(f.r.cfg.DataDir, "caches.json"), metadata)
			}
			for _, n := range []string{"unknown", "attached", "recent", "old", "not-cache", "unmanaged"} {
				writeFile(t, f.bin+".cache."+n, "sentinel")
			}
			free := func() (int64, error) {
				if _, err := os.Stat(f.bin + ".cache.unknown"); os.IsNotExist(err) {
					return 100, nil
				}
				return 0, nil
			}
			if !f.r.reclaimCaches(context.Background(), free, 100) {
				t.Fatal("lost cache was not discovered and reclaimed")
			}
			for _, n := range []string{"attached", "not-cache", "unmanaged"} {
				if b, err := os.ReadFile(f.bin + ".cache." + n); err != nil || string(b) != "sentinel" {
					t.Fatalf("removed protected %s: %q %v", n, b, err)
				}
			}
			if metadata == `{"recent":100,"old":1}` {
				for _, n := range []string{"old", "recent"} {
					if _, err := os.Stat(f.bin + ".cache." + n); err != nil {
						t.Fatalf("lost existing timestamp for %s", n)
					}
				}
				if !f.r.reclaimCaches(context.Background(), func() (int64, error) {
					if _, err := os.Stat(f.bin + ".cache.old"); os.IsNotExist(err) {
						return 100, nil
					}
					return 0, nil
				}, 100) {
					t.Fatal("known old cache not reclaimed")
				}
				if _, err := os.Stat(f.bin + ".cache.recent"); err != nil {
					t.Fatal("valid timestamp ordering lost")
				}
			}
		})
	}
}

func TestCacheDiscoveryFailureRetainsKnown(t *testing.T) {
	f := newFinishFixture(t)
	writeFile(t, filepath.Join(f.r.cfg.DataDir, "caches.json"), `{"known":1}`)
	writeFile(t, f.bin+".cache.known", "sentinel")
	writeFile(t, f.bin+".cache.unknown", "sentinel")
	writeFile(t, f.bin, `#!/bin/sh
case "$1 $2" in
"volume ls") echo unavailable >&2; exit 1 ;;
"volume rm") rm "$0.cache.$3" ;;
*) exit 1 ;;
esac
`)
	if !f.r.reclaimCaches(context.Background(), func() (int64, error) { return 0, nil }, 100) {
		t.Fatal("listing failure lost known cache")
	}
	if b, err := os.ReadFile(f.bin + ".cache.unknown"); err != nil || string(b) != "sentinel" {
		t.Fatal("listing failure removed unknown cache", err)
	}
}

func TestCachePlacementHoldsUntilLastFailedStart(t *testing.T) {
	f := newFinishFixture(t)
	for _, d := range []string{f.bin + ".root", f.bin + ".volumes"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	readyPath := f.bin + ".ready"
	if err := syscall.Mkfifo(readyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	arrived := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(ready)
		for scanner.Scan() {
			arrived <- scanner.Text()
		}
	}()
	script := strings.Replace(podmanForAStart, `"volume exists") exit 1 ;;
"volume create") ;;`, `"volume exists") test -d "$0.volumes/$3" ;;
"volume create") for n; do :; done; mkdir -p "$0.volumes/$n" ;;
"volume inspect") echo "$0.volumes/$5" ;;
"volume ls") ;;
"volume rm")
  shift 2
  for arg; do case "$arg" in -f|--force) ;; *) n="$arg" ;; esac; done
  rm -r "$0.volumes/$n" ;;`, 1)
	script = strings.Replace(script, "create) echo ctr-1 > \"$0.ctr\"; echo ctr-1 ;;", `create)
  shift
  while [ "$1" != --name ]; do shift; done
  n="$2"
  echo "$n" > "$0.ready"
  read release < "$0.release.$n"
  echo "fixture refused create" >&2
  exit 1 ;;`, 1)
	writeFile(t, f.bin, script)
	sp := spec.RunSpec{Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true},
		Volumes: []spec.Volume{{Name: "gomod", Path: "/cache", Kind: "cache"}}}
	ctx := context.Background()
	var placements []*placement
	for _, id := range []string{"cache-a", "cache-b"} {
		if err := syscall.Mkfifo(f.bin+".release.lux-"+id, 0o600); err != nil {
			t.Fatal(err)
		}
		p := newPlacement(f.r, proto.Assign{RunID: id, TenantID: "t1", Epoch: 1, Spec: sp}, nil)
		placements = append(placements, p)
		go p.run(ctx)
	}
	release := func(p *placement) {
		t.Helper()
		pipe, err := os.OpenFile(f.bin+".release.lux-"+p.runID, os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(pipe, "fail")
		pipe.Close()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Fatal("placement did not finish")
		}
	}
	for range placements {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("placement did not reach pre-create barrier")
		}
	}
	defer func() {
		for _, p := range placements {
			select {
			case <-p.done:
			default:
				release(p)
			}
		}
	}()
	sentinel := filepath.Join(f.bin+".volumes", cacheVolume("t1", "gomod", 0, 0), "module")
	writeFile(t, sentinel, "cached")
	reclaim := func() { f.r.reclaimCaches(ctx, func() (int64, error) { return 0, nil }, 100) }
	check := func() {
		t.Helper()
		if b, err := os.ReadFile(sentinel); err != nil || string(b) != "cached" {
			t.Fatalf("placement cache lost: %q %v", b, err)
		}
	}
	reclaim()
	check()
	release(placements[0])
	reclaim()
	check()
	release(placements[1])
	reclaim()
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("cache not reclaimed after last placement: %v", err)
	}
}

func TestCacheNeverInSnapshot(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.state.Volumes = append(f.p.state.Volumes, volumeRef{Name: "gomod", Volume: "cache", Path: "/cache", Kind: "cache"})
	f.p.assign.Spec.Artifacts.Paths = []string{"/cache/**"}
	f.r.mounts.Store("cache", f.data)
	sd, err := f.p.snapshot(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.Artifacts) != 0 {
		t.Fatalf("cache uploaded as artifacts: %+v", sd.Artifacts)
	}
	if len(sd.Manifest.Volumes) != 1 || sd.Manifest.Volumes[0].Name != "data" {
		t.Fatalf("manifest: %+v", sd.Manifest)
	}
}

func TestCacheReclaimLRUAndMounted(t *testing.T) {
	f := newFinishFixture(t)
	script := `#!/bin/sh
case "$1 $2" in
"volume rm")
  shift 2
  force=false
  n=
  for arg; do
    case "$arg" in -f|--force) force=true ;; *) n="$arg" ;; esac
  done
  [ -e "$0.busy.$n" ] && [ "$force" = false ] && exit 1
  rm "$0.cache.$n" ;;
*) exit 1 ;;
esac
`
	writeFile(t, f.bin, script)
	u := f.r.cacheUse()
	u.last = map[string]int64{"old": 1, "new": 2, "mounted": 0, "starting": 0}
	u.held["starting"] = 1
	for n := range u.last {
		writeFile(t, f.bin+".cache."+n, "cache")
	}
	writeFile(t, f.bin+".busy.mounted", "")
	free := func() (int64, error) {
		if _, err := os.Stat(f.bin + ".cache.old"); os.IsNotExist(err) {
			return 100, nil
		}
		return 0, nil
	}
	if !f.r.reclaimCaches(context.Background(), free, 100) {
		t.Fatal("nothing reclaimed")
	}
	for _, n := range []string{"new", "mounted", "starting"} {
		if _, err := os.Stat(f.bin + ".cache." + n); err != nil {
			t.Fatalf("removed %s", n)
		}
	}
	if _, err := os.Stat(f.bin + ".cache.old"); !os.IsNotExist(err) {
		t.Fatal("old remains")
	}
}

func TestCacheExcludedFromRunDisk(t *testing.T) {
	f := newFinishFixture(t)
	f.p.state.Volumes = []volumeRef{{Name: "cache", Volume: "cache", Path: "/cache", Kind: "cache"}}
	f.r.mounts.Store("cache", f.data)
	f.p.sampleSlow(context.Background())
	if got := f.p.usage(); got != nil && got.PeakDiskBytes != 0 {
		t.Fatalf("cache charged to Run: %+v", got)
	}
}

func TestSpotReleaseKeepsCache(t *testing.T) {
	f := newFinishFixture(t)
	writeFile(t, f.bin+".vol.cache", "")
	if err := f.p.removeWorkloadDisk(context.Background(), "", []volumeRef{{Volume: "cache", Kind: "cache"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.bin + ".vol.cache"); err != nil {
		t.Fatal("cache removed", err)
	}
}
