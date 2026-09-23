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
	if res.Base != base || res.Branch != "main" {
		t.Fatalf("got %+v", res)
	}
	// Work in the checkout survives a second Materialize (a resume).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\n"), 0o644)
	if _, err := m.Materialize(context.Background(), r, dir); err != nil {
		t.Fatal(err)
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
