package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcioapm/lux/internal/passwd"
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
"volume rm") [ -e "$0.busy.$3" ] && exit 1; rm "$0.cache.$3" ;;
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
