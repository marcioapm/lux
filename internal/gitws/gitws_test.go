package gitws

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A local "remote" (a bare repo on disk): exercises materialize and push
// without a network or credentials.
func remote(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "remote.git")
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(work, 0o755)
	run(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644)
	run(work, "add", "-A")
	run(work, "commit", "-qm", "one")
	run(root, "clone", "-q", "--bare", work, bare)
	return bare, run(work, "rev-parse", "HEAD")
}

func TestMaterializeIsIdempotentAndKeepsWork(t *testing.T) {
	bare, base := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "repos", "r")
	r := Repo{Name: "r", URL: bare, Ref: "main"}
	res, err := m.Materialize(context.Background(), r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != base || res.Branch != "main" || !res.Cloned {
		t.Fatalf("got %+v", res)
	}
	// Work in the checkout survives a second Materialize (a resume).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\n"), 0o644)
	if res, err := m.Materialize(context.Background(), r, dir); err != nil || res.Cloned {
		t.Fatalf("second materialize: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "mine\n" {
		t.Fatalf("work lost: %q", b)
	}
	// The clone stands alone: no alternates pointing at the host's mirror.
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("the checkout depends on the mirror")
	}
}

func TestUnknownRef(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	_, err := m.Materialize(context.Background(), Repo{Name: "r", URL: bare, Ref: "nope"}, filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Fatalf("got %v", err)
	}
}

func TestScrub(t *testing.T) {
	if got := Scrub("https://user:tok@example.com/a.git"); got != "https://example.com/a.git" {
		t.Fatal(got)
	}
}

// A push reads only the bundle the workload made: a hostile checkout's
// hooks and config (which could run code or redirect the push) are never
// consulted, because the runner never runs git in it.
func TestPushIgnoresTheCheckoutsGitConfig(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := m.Materialize(context.Background(), Repo{Name: "r", URL: bare, Ref: "main"}, dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(dir, ".git", "hooks")
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	cfg, _ := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	cfg.WriteString("[url \"file:///nonexistent/\"]\n\tinsteadOf = " + bare + "\n")
	cfg.Close()

	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitIn(t, dir, "bundle", "create", bundle, "HEAD")
	res := m.Push(context.Background(), Repo{Name: "r", URL: bare}, bundle, "lux/x", "")
	if res.Status != "pushed" {
		t.Fatalf("push: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checkout's pre-push hook ran")
	}
	// Leased: a new commit pushed with a stale lease (the branch is not
	// where this Run left it) is rejected, and the branch keeps its commit.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qam", "two")
	bundle2 := filepath.Join(t.TempDir(), "b2.bundle")
	gitIn(t, dir, "bundle", "create", bundle2, "HEAD")
	if res := m.Push(context.Background(), Repo{Name: "r", URL: bare}, bundle2, "lux/x", ""); res.Status != "rejected" {
		t.Fatalf("stale lease: %+v", res)
	}
	if res := m.Push(context.Background(), Repo{Name: "r", URL: bare}, bundle2, "lux/x", res.Commit); res.Status != "pushed" {
		t.Fatalf("correct lease: %+v", res)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// A clone that fails part way (here: an unknown ref) leaves nothing a
// later start would take for a checkout.
func TestFailedCloneLeavesNothing(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := m.Materialize(context.Background(), Repo{Name: "r", URL: bare, Ref: "nope"}, dir); err == nil {
		t.Fatal("expected an error")
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if res, err := m.Materialize(context.Background(), Repo{Name: "r", URL: bare, Ref: "main"}, dir); err != nil || !res.Cloned {
		t.Fatalf("retry: %+v %v", res, err)
	}
}
