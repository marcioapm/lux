package gitws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Three of seven dude reviewers starting together on one host failed with
// "git clone: ... trying to write ref ... with nonexistent object"
// (ABS-30, 2026-10-23). A fetch into the mirror ended with git's automatic
// gc, detached by default, so it was still packing loose objects and
// deleting them after the fetch had returned and released the mirror. A
// local clone lists refs first and then copies objects/ in directory
// order, so one copying meanwhile missed objects that moved into a pack it
// had already listed past. Reproduced with plain git: repack -d plus
// prune-packed during a clone of a large mirror fails 5 runs in 10.
//
// These tests pin the two halves of the fix: a fetch leaves no gc running
// behind it, and nothing writes to a mirror while a Run reads it.

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

func TestFetchRepacksBeforeReleasingTheMirror(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	r := Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}
	if _, err := m.Materialize(context.Background(), r, filepath.Join(t.TempDir(), "warm")); err != nil {
		t.Fatal(err)
	}
	mirror := m.mirrorPath("t", bare)
	// The mirror's own config asks for what git does by default on a busy
	// host: a gc after the fetch, detached. A fetch kept as its own pack
	// next to the clone's makes two packs, past the limit of one, so the
	// fetch's own `maintenance run --auto --detach` has a gc to do.
	gitRun(t, mirror, "config", "fetch.unpackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoPackLimit", "1")
	gitRun(t, mirror, "config", "gc.autoDetach", "true")
	gitRun(t, mirror, "config", "maintenance.autoDetach", "true")
	gitRun(t, mirror, "repack", "-q", "-a", "-d")
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("setup: the warm mirror should be one pack: %s", out)
	}
	pushLoose(t, bare, "next", 300)
	_, release, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "next"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Done when mirror returned: no gc still running, and the fetch's pack
	// already folded in.
	if _, err := os.Stat(filepath.Join(mirror, "gc.pid")); err == nil {
		t.Fatal("a gc is still running on the mirror after the fetch returned")
	}
	if out := gitRun(t, mirror, "count-objects", "-v"); !strings.Contains(out, "packs: 1\n") {
		t.Fatalf("the fetch left its repack to a detached gc: %s", out)
	}
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "next"}, filepath.Join(t.TempDir(), "next")); err != nil {
		t.Fatal(err)
	}
}

func TestAReaderHoldsOffAFetch(t *testing.T) {
	bare, sha := remote(t)
	m := New(t.TempDir())
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, filepath.Join(t.TempDir(), "warm")); err != nil {
		t.Fatal(err)
	}
	// A sha the mirror has: served without a fetch, read-locked.
	_, release, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: sha})
	if err != nil {
		t.Fatal(err)
	}
	// A second reader is not held up by the first.
	_, release2, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: sha})
	if err != nil {
		t.Fatal(err)
	}
	release2()
	pushLoose(t, bare, "next", 3)
	fetched := make(chan error, 1)
	go func() {
		_, rel, err := m.mirror(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "next"})
		if err == nil {
			rel()
		}
		fetched <- err
	}()
	select {
	case err := <-fetched:
		t.Fatalf("a fetch ran while a reader held the mirror (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if out := gitRun(t, m.mirrorPath("t", bare), "for-each-ref", "refs/heads/next"); out != "" {
		t.Fatalf("the mirror changed under a reader: %s", out)
	}
	release()
	select {
	case err := <-fetched:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fetch never ran after the reader released the mirror")
	}
}
