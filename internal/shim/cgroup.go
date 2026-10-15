package shim

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/marcioapm/lux/internal/proto"
)

// The Run's cgroup, as the shim sees it: the container has a private
// cgroup namespace, so /sys/fs/cgroup is the Run's own cgroup, whose
// memory.max, cpu.max and pids.max are the Run's limits.
//
// For a Run with nested containers (ShimConfig.DelegateCgroup) the shim
// splits it, so a container engine in the workload can give its containers
// limits of their own (docs/podman.md#limits-of-nested-containers):
//
//	/                 Run limits; no processes; controllers enabled below
//	├── lux.shim/     the shim (PID 1) and its own helpers
//	└── workload/     no processes; controllers enabled below; delegated
//	    └── main/     delegated; every tenant process starts here
//
// cgroup v2 allows controllers to be enabled only in a cgroup with no
// processes of its own, hence the shim's leaf, and workload/ holding none
// (an engine makes its containers' cgroups beside main/). Delegated means
// the directory and its cgroup.procs, cgroup.threads and
// cgroup.subtree_control belong to the workload user; resource files
// (memory.max, cgroup.max.descendants, …) stay the Run root's, so nothing
// the workload does can raise a limit lux set.
const (
	cgroupRoot     = "/sys/fs/cgroup"
	shimCgroup     = "lux.shim"
	workloadCgroup = "workload"
	mainCgroup     = "workload/main"
	// maxWorkloadDescendants bounds the cgroups the workload can make
	// below workload/: an engine makes one or two per container, so this
	// leaves room for hundreds of containers while keeping a workload from
	// filling kernel memory with empty cgroups.
	maxWorkloadDescendants = 1024
)

// delegatedControllers are enabled below the Run's cgroup and below
// workload/, those the Run's cgroup has; requiredControllers must all be
// there, or the shim does not split the cgroup.
var (
	delegatedControllers = []string{"cpu", "memory", "pids", "io"}
	requiredControllers  = []string{"cpu", "memory", "pids"}
)

// delegationFiles are what the workload user is given of workload/ and
// workload/main, beside the directory itself.
var delegationFiles = []string{"cgroup.procs", "cgroup.threads", "cgroup.subtree_control"}

// errNotDelegable: the Run's cgroup cannot be split here; the Run goes on
// in one cgroup.
var errNotDelegable = errors.New("cgroup not delegable")

// delegableControllers checks that the cgroup at root can be split: its
// cgroup.subtree_control is writable and it has every required controller.
// It returns the controllers to enable, in delegatedControllers' order.
func delegableControllers(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotDelegable, err)
	}
	have := strings.Fields(string(b))
	var missing []string
	for _, c := range requiredControllers {
		if !slices.Contains(have, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: controllers %s missing (have %q)", errNotDelegable, strings.Join(missing, ", "), strings.TrimSpace(string(b)))
	}
	if err := unix.Access(filepath.Join(root, "cgroup.subtree_control"), unix.W_OK); err != nil {
		return nil, fmt.Errorf("%w: cgroup.subtree_control: %v", errNotDelegable, err)
	}
	var enable []string
	for _, c := range delegatedControllers {
		if slices.Contains(have, c) {
			enable = append(enable, c)
		}
	}
	return enable, nil
}

// subtreeControl is what cgroup.subtree_control is written to enable cs.
func subtreeControl(cs []string) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = "+" + c
	}
	return strings.Join(parts, " ")
}

// cgroupPids are the pids in a cgroup.procs file.
func cgroupPids(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// delegateCgroup splits the cgroup at root for a workload running as
// uid:gid and returns an open descriptor of workload/main, for
// CLONE_INTO_CGROUP. probe starts and waits for a process cloned into a
// cgroup by descriptor: one that fails means this kernel or seccomp
// profile refuses it, and nothing is changed. Any error leaves the shim
// where it was, or in lux.shim, and tenant processes start beside it.
func delegateCgroup(root string, uid, gid int, probe func(fd int) error) (int, error) {
	enable, err := delegableControllers(root)
	if err != nil {
		return -1, err
	}
	for _, d := range []string{shimCgroup, workloadCgroup, mainCgroup} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return -1, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	fd, err := unix.Open(filepath.Join(root, mainCgroup), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", mainCgroup, err)
	}
	fail := func(err error) (int, error) {
		unix.Close(fd)
		return -1, err
	}
	if err := probe(fd); err != nil {
		for _, d := range []string{mainCgroup, workloadCgroup, shimCgroup} {
			_ = unix.Rmdir(filepath.Join(root, d))
		}
		return fail(fmt.Errorf("starting a process into %s: %w", mainCgroup, err))
	}
	if err := moveProcs(root, filepath.Join(root, shimCgroup)); err != nil {
		return fail(err)
	}
	ctl := subtreeControl(enable)
	for _, d := range []string{root, filepath.Join(root, workloadCgroup)} {
		if err := os.WriteFile(filepath.Join(d, "cgroup.subtree_control"), []byte(ctl), 0o644); err != nil {
			return fail(fmt.Errorf("enabling %q: %w", ctl, err))
		}
	}
	max := strconv.Itoa(maxWorkloadDescendants)
	if err := os.WriteFile(filepath.Join(root, workloadCgroup, "cgroup.max.descendants"), []byte(max), 0o644); err != nil {
		return fail(fmt.Errorf("%s/cgroup.max.descendants: %w", workloadCgroup, err))
	}
	if uid != 0 || gid != 0 {
		for _, d := range []string{workloadCgroup, mainCgroup} {
			if err := chownDelegated(filepath.Join(root, d), uid, gid); err != nil {
				return fail(err)
			}
		}
	}
	return fd, nil
}

// moveProcs moves every process of the cgroup at from into the cgroup at
// to. A process forked meanwhile lands in from too, so it reads again
// until from is empty, for a bounded time.
func moveProcs(from, to string) error {
	for range 50 {
		pids, err := cgroupPids(filepath.Join(from, "cgroup.procs"))
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			// ESRCH: it exited meanwhile.
			if err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("moving %d to %s: %w", pid, filepath.Base(to), err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("processes keep appearing in %s", from)
}

// chownDelegated gives the cgroup directory dir and its delegationFiles to
// uid:gid, through dir's descriptor, never following a link.
func chownDelegated(dir string, uid, gid int) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	for _, f := range delegationFiles {
		if err := unix.Fchownat(fd, f, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("chown %s/%s: %w", dir, f, err)
		}
	}
	return nil
}

// probeCloneInto starts `lux-shim capabilities`, cloned into the cgroup
// fd, and waits for it. Called before the reaper runs, so Wait reaps it.
func probeCloneInto(fd int) error {
	cmd := exec.Command(proto.ShimBinary, proto.ShimCapabilitiesCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	return cmd.Run()
}

// delegate splits the Run's cgroup when its config asks, and keeps the
// workload/main descriptor tenant processes are started into. When it
// cannot, it says so once and the Run goes on in one cgroup, as one
// without nested containers.
func (s *Shim) delegate() {
	if !s.cfg.DelegateCgroup {
		return
	}
	uid, gid := 0, 0
	u, err := lookupUser(s.cfg.User)
	if err == nil {
		uid, gid = u.uid, u.gid
	}
	if err == nil {
		var fd int
		if fd, err = delegateCgroup(cgroupRoot, uid, gid, probeCloneInto); err == nil {
			s.workCgroup = os.NewFile(uintptr(fd), filepath.Join(cgroupRoot, mainCgroup))
		}
	}
	if err != nil {
		msg := "nested containers: the Run's cgroup is not delegated, so limits given to nested containers are not enforced: " + err.Error()
		fmt.Fprintln(os.Stderr, "lux-shim:", msg)
		s.out.Event(proto.EvWarning, map[string]any{"message": msg})
	}
}

// intoWorkloadCgroup makes cmd start in workload/main when the Run's
// cgroup is delegated.
func (s *Shim) intoWorkloadCgroup(cmd *exec.Cmd) {
	if s.workCgroup != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(s.workCgroup.Fd())
	}
}

// memoryOOM is the oom count in a memory.events.local file: how often
// that cgroup's own memory.max was reached with nothing left to reclaim.
// nil when the file cannot be read or has no such key.
func memoryOOM(path string) *int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok || k != "oom" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	return nil
}
