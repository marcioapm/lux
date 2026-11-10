package shim

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// writeFiles writes the spec's files (cfg.Files) into place, before init,
// then lets go of their bytes. It works as the workload user: a file goes
// only where that user could write it, and it and the directories made for
// it are that user's. No symbolic link is followed: one on the way fails
// the start, and one at a file's own path is replaced, never written
// through.
func (s *Shim) writeFiles() error {
	files := s.cfg.Files
	s.cfg.Files = nil
	if len(files) == 0 {
		return nil
	}
	var written int
	done := make(chan error, 1)
	go func() {
		// Never unlocked: the thread takes the workload user's file
		// credentials, and goes with this goroutine.
		runtime.LockOSThread()
		if err := s.actAsWorkload(); err != nil {
			done <- err
			return
		}
		for _, f := range files {
			p := spec.ExpandFilePath(f.Path, s.user.home)
			// ~/ is only known here: a home of / would put it anywhere.
			if msg := spec.CheckFileTarget(p, s.cfg.Secrets); msg != "" {
				done <- fmt.Errorf("%s: %s", f.Path, msg)
				return
			}
			n, err := placeFile(p, f)
			if err != nil {
				done <- fmt.Errorf("%s: %w", f.Path, err)
				return
			}
			written += n
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		return err
	}
	s.out.Event(proto.EvFiles, map[string]any{"count": len(files), "bytes": written})
	return nil
}

// actAsWorkload gives the calling thread, locked to its goroutine, the
// workload user's file system identity: its groups, fsgid and fsuid. A
// non-root fsuid drops root's file capabilities (CAP_DAC_OVERRIDE,
// CAP_FOWNER, CAP_CHOWN, ...) on that thread. A shim that is not root (a
// test) stays who it is.
func (s *Shim) actAsWorkload() error {
	if os.Geteuid() != 0 || s.user.uid == 0 {
		return nil
	}
	groups := make([]int, len(s.user.groups))
	for i, g := range s.user.groups {
		groups[i] = int(g)
	}
	// x/sys's setgroups is the bare system call: this thread's alone.
	if err := unix.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setfsgid(s.user.gid); err != nil {
		return fmt.Errorf("setfsgid: %w", err)
	}
	if err := unix.Setfsuid(s.user.uid); err != nil {
		return fmt.Errorf("setfsuid: %w", err)
	}
	// setfsuid and setfsgid report no failure: read them back.
	if uid, _ := unix.SetfsuidRetUid(-1); uid != s.user.uid {
		return fmt.Errorf("setfsuid: still %d", uid)
	}
	if gid, _ := unix.SetfsgidRetGid(-1); gid != s.user.gid {
		return fmt.Errorf("setfsgid: still %d", gid)
	}
	return nil
}

// placeFile writes f's bytes at p (absolute, clean) and returns how many:
// its directory is opened from / one component at a time without
// following links, missing ones made 0755; the bytes go to a new file
// there, which is given f's mode and renamed over p.
func placeFile(p string, f spec.File) (int, error) {
	dir, err := openDirNoFollow(p[:strings.LastIndex(p, "/")+1])
	if err != nil {
		return 0, err
	}
	defer unix.Close(dir)
	name := p[strings.LastIndex(p, "/")+1:]
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".lux-file-" + hex.EncodeToString(rnd[:])
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(fd), tmp)
	b := f.Decoded()
	_, err = file.Write(b)
	if err == nil {
		// Set outright: the open's mode went through the umask.
		err = file.Chmod(os.FileMode(f.Mode))
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// Replaces whatever is at the name, a link included, and never
		// what it points to; a directory there refuses it.
		err = unix.Renameat(dir, tmp, dir, name)
	}
	if err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return 0, fmt.Errorf("a directory is in the way")
		}
		return 0, err
	}
	return len(b), nil
}

// openDirNoFollow opens directory d (absolute, ending in /), making what
// is missing (0755). A component that is a link, or not a directory, fails
// it.
func openDirNoFollow(d string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	at := ""
	for _, part := range strings.Split(strings.Trim(d, "/"), "/") {
		if part == "" {
			continue
		}
		at += "/" + part
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			made := unix.Mkdirat(fd, part, 0o755)
			if made == nil || errors.Is(made, unix.EEXIST) {
				next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			} else {
				err = made
			}
			// 0755 outright: mkdir's mode went through the umask.
			if made == nil && err == nil {
				err = unix.Fchmod(next, 0o755)
			}
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			var st unix.Stat_t
			if unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
				err = fmt.Errorf("%s is a symbolic link: lux does not follow links (name the path it leads to)", at)
			} else {
				err = fmt.Errorf("%s is not a directory", at)
			}
		} else if err != nil {
			err = fmt.Errorf("%s: %w", at, err)
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}
