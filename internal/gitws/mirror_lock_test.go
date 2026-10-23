package gitws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Three of seven dude reviewers starting together on one host failed with
// "git clone: ... trying to write ref ... with nonexistent object"
// (ABS-30, 2026-10-23). A fetch into the mirror ended with git's automatic
// gc, detached by default, so it was still packing loose objects and
// deleting them after the fetch had returned. A local clone lists refs
// first and then copies objects/ in directory order, so one copying
// meanwhile missed objects that moved into a pack it had already gone
// past. Plain git reproduces it: repack -d plus prune-packed during a
// clone of a large mirror fails 5 runs in 10.
//
// These tests pin the fix: a fetch leaves no gc running behind it, gc
// waits for readers, a clone holds the mirror until it has copied it, and
// clones beside fetches and repacks come out whole.

// pushLoose adds a commit of n new files to the remote.
func pushLoose(t *testing.T, bare, branch string, n int) {
	t.Helper()
	work := filepath.Join(filepath.Dir(bare), "work")
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(work, fmt.Sprintf("%s-%d.txt", branch, i)), []byte(branch+fmt.Sprint(i)), 0o644)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", branch)
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/"+branch)
}

// warm materializes main once, so the mirror exists, and returns its path.
func warm(t *testing.T, m *Manager, bare string) string {
	t.Helper()
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, filepath.Join(t.TempDir(), "warm")); err != nil {
		t.Fatal(err)
	}
	return m.mirrorPath("t", bare)
}

// fetch runs mirror() for ref and releases it at once.
func fetch(t *testing.T, m *Manager, bare, ref string) {
	t.Helper()
	_, release, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestFetchRepacksBeforeReturning(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	// Forces the auto gc a busy mirror reaches by default, detached as git
	// would run it: a fetch kept as its own pack next to the mirror's one
	// makes two, past the limit of one.
	gitRun(t, mirror, "config", "fetch.unpackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoDetach", "true")
	gitRun(t, mirror, "config", "maintenance.autoDetach", "true")
	gitRun(t, mirror, "repack", "-q", "-a", "-d")
	pushLoose(t, bare, "next", 300)
	fetch(t, m, bare, "next")
	// Done when mirror returned: no gc still running, the fetch's pack
	// already folded in.
	if _, err := os.Stat(filepath.Join(mirror, "gc.pid")); err == nil {
		t.Fatal("a gc is still running on the mirror after the fetch returned")
	}
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("the fetch left its repack to a detached gc: %s", out)
	}
}

// A reader holds off gc, not other readers and not a fetch; gc runs on the
// next fetch once the mirror is free.
func TestAReaderHoldsOffGCOnly(t *testing.T) {
	bare, sha := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	gitRun(t, mirror, "config", "fetch.unpackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	gitRun(t, mirror, "repack", "-q", "-a", "-d")
	l := m.lockFor(mirror)
	_, release, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !l.gc.TryRLock() {
		t.Fatal("a second reader was held up by the first")
	}
	l.gc.RUnlock()
	if l.gc.TryLock() {
		l.gc.Unlock()
		t.Fatal("gc could run while a reader held the mirror")
	}
	// A fetch still runs (it only adds objects), and skips its gc.
	pushLoose(t, bare, "next", 3)
	fetched := make(chan struct{})
	go func() { defer close(fetched); fetch(t, m, bare, "next") }()
	select {
	case <-fetched:
	case <-time.After(30 * time.Second):
		release()
		<-fetched
		t.Fatal("a fetch waited for a reader")
	}
	if out := gitRun(t, mirror, "for-each-ref", "refs/heads/next"); out == "" {
		t.Fatal("the fetch did not run beside a reader")
	}
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 2\n") {
		t.Fatalf("gc ran while a reader held the mirror: %s", out)
	}
	release()
	pushLoose(t, bare, "later", 3)
	fetch(t, m, bare, "later")
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("gc did not run once the mirror was free: %s", out)
	}
}

// A clone holds the mirror until it has copied it. A FIFO in objects/ parks
// the clone mid-copy: opening it for writing returns once the clone opened
// it for reading.
func TestMaterializeHoldsTheMirrorThroughItsClone(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	fifo := filepath.Join(mirror, "objects", "info", "park")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(fifo)
	done := make(chan error, 1)
	go func() {
		_, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, filepath.Join(t.TempDir(), "c"))
		done <- err
	}()
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	l := m.lockFor(mirror)
	if l.gc.TryLock() {
		l.gc.Unlock()
		w.Close()
		t.Fatal("gc could run while a clone was copying the mirror")
	}
	w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the clone never finished")
	}
	if !l.gc.TryLock() {
		t.Fatal("the clone kept the mirror after it finished")
	}
	l.gc.Unlock()
}

// The symptom end to end: clones of a mirror while fetches into it each
// leave loose objects and make a repack due. Probabilistic (main fails a
// few of 60), so it complements the deterministic tests above.
func TestClonesSurviveConcurrentFetchAndRepack(t *testing.T) {
	if testing.Short() {
		t.Skip("about 30 s")
	}
	bare, _ := remote(t)
	work := filepath.Join(filepath.Dir(bare), "work")
	for c := 0; c < 20; c++ {
		for i := 0; i < 200; i++ {
			os.WriteFile(filepath.Join(work, fmt.Sprintf("f%d-%d.txt", c, i)), []byte(fmt.Sprint(c, i)), 0o644)
		}
		gitRun(t, work, "add", "-A")
		gitRun(t, work, "commit", "-qm", "bulk")
	}
	gitRun(t, work, "push", "-q", bare, "HEAD:refs/heads/main")
	m := New(t.TempDir())
	mirror := warm(t, m, bare)
	gitRun(t, mirror, "config", "fetch.unpackLimit", "100000")
	gitRun(t, mirror, "config", "gc.auto", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	stop := make(chan struct{})
	fetched := make(chan struct{})
	go func() {
		defer close(fetched)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			pushLoose(t, bare, fmt.Sprintf("b%d", n), 300)
			if _, release, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}); err == nil {
				release()
			}
		}
	}()
	root := t.TempDir()
	var failed []error
	for i := 0; i < 60; i++ {
		if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, filepath.Join(root, fmt.Sprint(i))); err != nil {
			failed = append(failed, err)
		}
	}
	close(stop)
	<-fetched
	if len(failed) > 0 {
		t.Fatalf("%d of 60 clones failed; first: %v", len(failed), failed[0])
	}
}
