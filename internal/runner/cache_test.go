package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	sp := spec.RunSpec{Volumes: []spec.Volume{{Name: "gomod", Path: "/cache", Kind: "cache"}}}
	prepare := func(run, tenant string) *placement {
		p := &placement{r: f.r, runID: run, tenantID: tenant, epoch: 1, dir: f.r.runDir(run), state: &runState{RunID: run}}
		if err := p.prepareVolumes(ctx, sp, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := prepare("a", "tenant-a")
	mp := filepath.Join(f.bin+".volumes", a.state.Volumes[0].Volume)
	writeFile(t, filepath.Join(mp, "module"), "cached")
	for _, c := range []struct {
		run, tenant string
		want        bool
	}{{"b", "tenant-a", true}, {"a", "tenant-a", true}, {"c", "tenant-b", false}} {
		p := prepare(c.run, c.tenant)
		b, err := os.ReadFile(filepath.Join(f.bin+".volumes", p.state.Volumes[0].Volume, "module"))
		if (err == nil) != c.want || c.want && string(b) != "cached" {
			t.Fatalf("%+v: %q %v", c, b, err)
		}
	}
}

func TestCacheNeverInSnapshot(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.state.Volumes = append(f.p.state.Volumes, volumeRef{Name: "gomod", Volume: "cache", Path: "/cache", Kind: "cache"})
	sd, err := f.p.snapshot(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
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
