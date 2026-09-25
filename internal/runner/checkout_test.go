package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A volume's contents are the workload's: a symlink it planted must never
// take a checkout (made by the runner, as root on the host) outside it.
func TestCheckoutDirStaysInItsVolume(t *testing.T) {
	base := t.TempDir()
	vol := filepath.Join(base, "vol")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{vol, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{}
	r.mounts.Store("lux-run-workspace", vol)
	p := &placement{r: r, state: &runState{Volumes: []volumeRef{{Name: "workspace", Volume: "lux-run-workspace", Path: "/workspace"}}}}

	mp, dir, err := p.checkoutDir("/workspace/repos/web")
	if err != nil || dir != filepath.Join(vol, "repos", "web") || mp != vol {
		t.Fatalf("plain: %s %s %v", mp, dir, err)
	}

	// repos -> a directory outside the volume.
	os.RemoveAll(filepath.Join(vol, "repos"))
	if err := os.Symlink(outside, filepath.Join(vol, "repos")); err != nil {
		t.Fatal(err)
	}
	if _, dir, err := p.checkoutDir("/workspace/repos/web"); err == nil {
		t.Fatalf("followed a symlink out of the volume to %s", dir)
	}
	os.Remove(filepath.Join(vol, "repos"))

	// repos -> ../outside, relative.
	if err := os.Symlink("../outside", filepath.Join(vol, "repos")); err != nil {
		t.Fatal(err)
	}
	if _, dir, err := p.checkoutDir("/workspace/repos/web"); err == nil {
		t.Fatalf("followed a relative symlink out to %s", dir)
	}
	os.Remove(filepath.Join(vol, "repos"))

	// The checkout itself a symlink.
	os.Mkdir(filepath.Join(vol, "repos"), 0o755)
	if err := os.Symlink(outside, filepath.Join(vol, "repos", "web")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.checkoutDir("/workspace/repos/web"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked checkout: %v", err)
	}

	// A symlink that stays inside is fine.
	os.RemoveAll(filepath.Join(vol, "repos"))
	os.Mkdir(filepath.Join(vol, "real"), 0o755)
	os.Symlink("real", filepath.Join(vol, "repos"))
	if _, dir, err := p.checkoutDir("/workspace/repos/web"); err != nil || dir != filepath.Join(vol, "real", "web") {
		t.Fatalf("inside: %s %v", dir, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was made outside: %v", entries)
	}
}
