package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

const imagePasswd = `root:x:0:0:root:/root:/bin/sh
runner:x:1000:1000::/home/runner:/bin/sh
run:x:1001:1001::/home/run:/bin/sh
`

const imageGroup = `root:x:0:runner
adm:x:4:runner
runner:x:1000:runner
run:x:1001:
docker:x:999:runner
`

// imageDir is an unpacked image with passwd and, unless group is empty,
// group in its etc.
func imageDir(t *testing.T, group string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte(imagePasswd), 0o644); err != nil {
		t.Fatal(err)
	}
	if group != "" {
		if err := os.WriteFile(filepath.Join(dir, "etc/group"), []byte(group), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// countGroupOpens calls f and counts the opens of dir's etc/group while it
// runs, through inotify on dir/etc: workloadUser has no hook for it.
func countGroupOpens(t *testing.T, dir string, f func()) int {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, filepath.Join(dir, "etc"), syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	f()
	// The kernel queues IN_OPEN before open returns: all are there now.
	n := 0
	buf := make([]byte, 64<<10)
	for {
		r, err := syscall.Read(fd, buf)
		if err == syscall.EAGAIN {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= r; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+int(ev.Len)]
			if string(bytes.TrimRight(name, "\x00")) == "group" {
				n++
			}
			off += syscall.SizeofInotifyEvent + int(ev.Len)
		}
	}
}

// Who the workload runs as and its groups, for the spec's user or the
// image's USER, as podman and runc give them (but root, which keeps its
// primary group only). Only an eligible user has /etc/group read.
func TestWorkloadUserGroups(t *testing.T) {
	dir := imageDir(t, imageGroup)
	root := openRoot(t, dir)
	for _, c := range []struct {
		specUser, imageUser string
		uid, gid            int
		groups              []int
		reads               int
	}{
		{"runner", "", 1000, 1000, []int{0, 4, 999}, 1},
		{"1000", "", 1000, 1000, []int{0, 4, 999}, 1},
		{"01000", "", 1000, 1000, []int{0, 4, 999}, 1},
		{"+1000", "", 1000, 1000, []int{0, 4, 999}, 1},
		{"runner:", "", 1000, 1000, []int{0, 4, 999}, 1},
		{"", "runner", 1000, 1000, []int{0, 4, 999}, 1},
		{"", "1000", 1000, 1000, []int{0, 4, 999}, 1},
		{"", "runner:", 1000, 1000, []int{0, 4, 999}, 1},
		{"run", "runner", 1001, 1001, nil, 1}, // the spec's user, in no group
		{"runner:999", "", 1000, 999, nil, 0}, // an explicit group: that group only
		{"1000:1000", "", 1000, 1000, nil, 0},
		{"", "runner:4", 1000, 4, nil, 0},
		{"4242", "", 4242, 4242, nil, 0}, // no entry
		{"", "", 0, 0, nil, 0},           // root
		{"root", "runner", 0, 0, nil, 0},
		{"0", "", 0, 0, nil, 0},
		{"00", "", 0, 0, nil, 0}, // root, numerically: no groups either
		{"0:0", "", 0, 0, nil, 0},
	} {
		sp := spec.RunSpec{Workload: spec.Workload{User: c.specUser}}
		var (
			u      passwd.User
			groups []int
			warn   string
			err    error
		)
		reads := countGroupOpens(t, dir, func() {
			u, groups, warn, err = workloadUser(sp, podman.ImageInfo{User: c.imageUser}, root)
		})
		if err != nil || warn != "" {
			t.Fatalf("%q/%q: %v %q", c.specUser, c.imageUser, err, warn)
		}
		want, _ := passwd.Lookup(map[bool]string{true: c.specUser, false: c.imageUser}[c.specUser != ""], []byte(imagePasswd))
		if u != want || u.UID != c.uid || u.GID != c.gid || !slices.Equal(groups, c.groups) || reads != c.reads {
			t.Errorf("%q/%q: %+v groups %v (%d reads); want %d:%d groups %v (%d reads), Lookup %+v",
				c.specUser, c.imageUser, u, groups, reads, c.uid, c.gid, c.groups, c.reads, want)
		}
	}
}

// A user not in the image's /etc/passwd still fails the start, as before.
func TestWorkloadUserUnknownName(t *testing.T) {
	root := openRoot(t, imageDir(t, imageGroup))
	if _, _, _, err := workloadUser(spec.RunSpec{Workload: spec.Workload{User: "nobody"}}, podman.ImageInfo{}, root); err == nil {
		t.Fatal("no error")
	}
}

// An /etc/group that is not a small regular file inside the image gives
// the primary group only, promptly, with a warning when the user has
// memberships to lose; never an error. A user with none to look up
// (explicit group, no entry) reads nothing and is not warned.
func TestImageGroupFileBounded(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "group")
	if err := os.WriteFile(outside, []byte(imageGroup), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, etc string)
		warn  string
	}{
		{"fifo", func(t *testing.T, etc string) {
			if err := syscall.Mkfifo(filepath.Join(etc, "group"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"symlink to a fifo in the image", func(t *testing.T, etc string) {
			if err := syscall.Mkfifo(filepath.Join(etc, "fifo"), 0o644); err != nil {
				t.Fatal(err)
			}
			symlink(t, "fifo", filepath.Join(etc, "group"))
		}, "not a regular file"},
		{"symlink to /dev/zero", func(t *testing.T, etc string) {
			symlink(t, "/dev/zero", filepath.Join(etc, "group"))
		}, "escapes"},
		{"symlink up to /dev/zero", func(t *testing.T, etc string) {
			symlink(t, strings.Repeat("../", 40)+"dev/zero", filepath.Join(etc, "group"))
		}, "escapes"},
		{"a directory", func(t *testing.T, etc string) {
			if err := os.Mkdir(filepath.Join(etc, "group"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"oversized", func(t *testing.T, etc string) {
			b := []byte(imageGroup + strings.Repeat("#", maxGroupFile))
			if err := os.WriteFile(filepath.Join(etc, "group"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "larger than"},
		{"oversized, sparse", func(t *testing.T, etc string) {
			p := filepath.Join(etc, "group")
			if err := os.WriteFile(p, []byte(imageGroup), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(p, 1<<40); err != nil {
				t.Fatal(err)
			}
		}, "larger than"},
		// A volume over /etc, or one restored from a snapshot, is not the
		// image: its group file grants nothing, even through a symlink.
		{"symlink into a volume", func(t *testing.T, etc string) {
			symlink(t, outside, filepath.Join(etc, "group"))
		}, "escapes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := imageDir(t, "")
			c.setup(t, filepath.Join(dir, "etc"))
			root := openRoot(t, dir)
			for _, user := range []string{"runner", "1000", "run", "runner:999", "4242"} {
				done := make(chan struct{})
				var (
					u      passwd.User
					groups []int
					warn   string
					err    error
				)
				go func() {
					defer close(done)
					u, groups, warn, err = workloadUser(spec.RunSpec{Workload: spec.Workload{User: user}}, podman.ImageInfo{}, root)
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("%s: workloadUser blocked", user)
				}
				want, _ := passwd.Lookup(user, []byte(imagePasswd))
				if err != nil || u != want || groups != nil {
					t.Fatalf("%s: %+v groups %v err %v; want %+v, none", user, u, groups, err, want)
				}
				// Only a user with an entry and no group has a file to read.
				if eligible := user == "runner" || user == "1000" || user == "run"; eligible != (warn != "") || (warn != "" && !strings.Contains(warn, c.warn)) {
					t.Fatalf("%s: warn %q, want %q", user, warn, c.warn)
				}
			}
		})
	}
}

// The groups are the image's even when a volume over /etc in the
// container would list others: the runner reads the image root only.
func TestImageGroupsIgnoreVolumeEtc(t *testing.T) {
	dir := imageDir(t, imageGroup)
	vol := t.TempDir()
	if err := os.WriteFile(filepath.Join(vol, "group"), []byte(imageGroup+"undeclared:x:1234:runner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The image's /etc itself, linked at the volume: os.Root refuses it.
	linked := t.TempDir()
	symlink(t, vol, filepath.Join(linked, "etc"))
	for _, c := range []struct {
		dir  string
		want []int
	}{{dir, []int{0, 4, 999}}, {linked, nil}} {
		_, groups, _, err := workloadUser(spec.RunSpec{Workload: spec.Workload{User: "runner"}}, podman.ImageInfo{}, openRoot(t, c.dir))
		if err != nil && c.dir == dir {
			t.Fatal(err)
		}
		if slices.Contains(groups, 1234) || !slices.Equal(groups, c.want) {
			t.Fatalf("%s: groups %v, want %v", c.dir, groups, c.want)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// The shim is given the uid:gid (the image's USER) or the spec's user as
// before, and the supplementary gids the runner read from the image.
func TestShimConfigGroups(t *testing.T) {
	for _, c := range []struct {
		name, specUser string
		user           passwd.User
		groups         []int
		wantUser       string
	}{
		{"image user", "", passwd.User{UID: 1000, GID: 1000}, []int{0, 4, 999}, "1000:1000"},
		{"image root", "", passwd.User{Name: "root"}, nil, "0:0"},
		{"spec user", "runner", passwd.User{UID: 1000, GID: 1000}, []int{0, 4, 999}, "runner"},
		{"spec user with a group", "runner:999", passwd.User{UID: 1000, GID: 999}, nil, "runner:999"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := t.TempDir()
			r := &Runner{mem: mustScale(t, 30*gib+gib/2, gib/2, 32*gib)}
			r.mounts.Store(runtimeVolume("run_1"), rt)
			p := &placement{r: r, runID: "run_1", epoch: 1, assign: &proto.Assign{}, state: &runState{}, user: c.user, groups: c.groups}
			sp := spec.RunSpec{Workload: spec.Workload{User: c.specUser}, Resources: spec.Resources{Memory: spec.Bytes(gib)}}
			if err := p.writeShimConfig(context.Background(), sp, nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(rt, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg proto.ShimConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.User != c.wantUser || !slices.Equal(cfg.Groups, c.groups) {
				t.Fatalf("user %q groups %v, want %q %v", cfg.User, cfg.Groups, c.wantUser, c.groups)
			}
			// No groups: no field, as an older runner writes it.
			if len(c.groups) == 0 && strings.Contains(string(b), `"groups"`) {
				t.Fatalf("groups field with none: %s", b)
			}
		})
	}
}
