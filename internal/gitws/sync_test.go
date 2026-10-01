package gitws

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/shim"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushCommit commits path=content on main in the remote, through a scratch
// clone; returns the new commit.
func pushCommit(t *testing.T, bare, path, content string) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w")
	gitRun(t, filepath.Dir(w), "clone", "-q", bare, w)
	os.WriteFile(filepath.Join(w, path), []byte(content), 0o644)
	gitRun(t, w, "add", "-A")
	gitRun(t, w, "commit", "-qm", "c")
	gitRun(t, w, "push", "-q", "origin", "HEAD:main")
	return gitRun(t, w, "rev-parse", "HEAD")
}

// The sync end to end, but for the container: the runner's bundle through
// the mirror, then lux-shim sync's rule on the checkout.
func TestSync(t *testing.T) {
	bare, base := remote(t)
	m := New(t.TempDir())
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repos", "r")
	r := Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}
	if _, err := m.Materialize(ctx, r, dir); err != nil {
		t.Fatal(err)
	}
	sync := func(ref string) proto.SyncResult {
		t.Helper()
		r.Ref = ref
		bundle := filepath.Join(t.TempDir(), "r.bundle")
		tg, err := m.SyncBundle(ctx, r, bundle)
		if err != nil {
			t.Fatal(err)
		}
		res := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: dir, Ref: ref, Commit: tg.Commit, Branch: tg.Branch, Bundle: bundle}}})
		return res[0]
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }

	// Nothing new: up to date.
	if res := sync("main"); res.Status != "up-to-date" || res.From != base || res.To != base {
		t.Fatalf("up to date: %+v", res)
	}
	// A new commit, a clean checkout with untracked and ignored files:
	// fast-forward, the files kept.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("data/\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "data", "counter"), []byte("7"), 0o644)
	c2 := pushCommit(t, bare, "a.txt", "two\n")
	res := sync("main")
	if res.Status != "fast-forward" || res.From != base || res.To != c2 || res.Dirty || res.Diverged {
		t.Fatalf("fast-forward: %+v", res)
	}
	if read("a.txt") != "two\n" || read("data/counter") != "7" || read(".gitignore") != "data/\n" ||
		gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatalf("after fast-forward: %q %q", read("a.txt"), read("data/counter"))
	}
	// A tracked file changed: reset to the ref, the change saved in
	// refs/lux/pre-sync, untracked files kept.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local edit\n"), 0o644)
	c3 := pushCommit(t, bare, "b.txt", "bee\n")
	res = sync("main")
	if res.Status != "reset" || !res.Dirty || res.Diverged || res.To != c3 || res.Saved != "refs/lux/pre-sync" {
		t.Fatalf("dirty: %+v", res)
	}
	if read("a.txt") != "two\n" || read("b.txt") != "bee\n" || read("data/counter") != "7" {
		t.Fatalf("after reset: %q %q", read("a.txt"), read("b.txt"))
	}
	if saved := gitRun(t, dir, "show", "refs/lux/pre-sync:a.txt"); saved != "local edit" {
		t.Fatalf("saved: %q", saved)
	}
	// A local commit the remote does not have (diverged): reset; the
	// local commit is refs/lux/pre-sync.
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("mine\n"), 0o644)
	gitRun(t, dir, "add", "c.txt")
	gitRun(t, dir, "commit", "-qm", "local")
	local := gitRun(t, dir, "rev-parse", "HEAD")
	c4 := pushCommit(t, bare, "a.txt", "four\n")
	res = sync("main")
	if res.Status != "reset" || res.Dirty || !res.Diverged || res.From != local || res.To != c4 {
		t.Fatalf("diverged: %+v", res)
	}
	if gitRun(t, dir, "rev-parse", "refs/lux/pre-sync") != local || read("a.txt") != "four\n" {
		t.Fatalf("after diverged reset")
	}
	// A sha: detached at it.
	res = sync(c2)
	if res.Status != "reset" || res.To != c2 || gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatalf("to a sha: %+v", res)
	}
	// An unknown ref: the runner's bundle fails; nothing changes.
	r.Ref = "nope"
	if _, err := m.SyncBundle(ctx, r, filepath.Join(t.TempDir(), "x.bundle")); err == nil || !strings.Contains(err.Error(), `ref "nope" not found`) {
		t.Fatalf("unknown ref: %v", err)
	}
	// A bundle of another commit than announced, or no checkout: failed,
	// the checkout as it was.
	r.Ref = "main"
	bundle := filepath.Join(t.TempDir(), "r.bundle")
	if _, err := m.SyncBundle(ctx, r, bundle); err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	got := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{
		{Name: "r", Path: dir, Ref: "main", Commit: base, Branch: "main", Bundle: bundle},
		{Name: "gone", Path: filepath.Join(t.TempDir(), "none"), Ref: "main", Commit: base, Bundle: bundle},
	}})
	if got[0].Status != "failed" || !strings.Contains(got[0].Error, "the bundle has") || got[1].Status != "failed" ||
		!strings.Contains(got[1].Error, "has no checkout") || gitRun(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("failures: %+v", got)
	}
}
