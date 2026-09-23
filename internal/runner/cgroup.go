package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// cgroupRoot is where the runner makes cgroups of its own (cgroup v2).
const cgroupRoot = "/sys/fs/cgroup"

// buildCgroup is a cgroup an image build runs under: it carries the Run's
// process limit, and killing it ends every process the build started.
type buildCgroup struct {
	path string // relative to cgroupRoot, as podman's --cgroup-parent takes it
}

func newBuildCgroup(runID string, res spec.Resources) (*buildCgroup, error) {
	parent := filepath.Join(cgroupRoot, "lux.slice")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("build cgroup: %w", err)
	}
	// The pids controller, for the cgroups below lux.slice (and so, first,
	// for lux.slice itself).
	for _, d := range []string{cgroupRoot, parent} {
		if err := os.WriteFile(filepath.Join(d, "cgroup.subtree_control"), []byte("+pids"), 0o644); err != nil {
			return nil, fmt.Errorf("build cgroup: enabling pids in %s: %w", d, err)
		}
	}
	cg := &buildCgroup{path: "/lux.slice/build-" + runID}
	dir := filepath.Join(cgroupRoot, cg.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("build cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(fmt.Sprint(res.Pids)), 0o644); err != nil {
		cg.remove()
		return nil, fmt.Errorf("build cgroup: pids.max: %w", err)
	}
	return cg, nil
}

// remove kills whatever is left in the cgroup and removes it (and the
// cgroups podman made below it).
func (cg *buildCgroup) remove() {
	dir := filepath.Join(cgroupRoot, cg.path)
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
	for range 50 {
		if removeTree(dir) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// removeTree removes a cgroup and its children, deepest first (cgroups are
// directories, but only rmdir removes them).
func removeTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := removeTree(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
