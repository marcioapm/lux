package shim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// modeRepo is an upstream (a working repository standing in for the
// host's mirror) and a checkout cloned from it, on branch main.
type modeRepo struct {
	t        *testing.T
	upstream string
	dir      string
}

func newModeRepo(t *testing.T) *modeRepo {
	t.Helper()
	up := filepath.Join(t.TempDir(), "up")
	gitT(t, filepath.Dir(up), "init", "-q", "-b", "main", up)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("one\n"), 0o644)
	gitT(t, up, "add", "-A")
	gitT(t, up, "commit", "-qm", "one")
	dir := filepath.Join(t.TempDir(), "co")
	gitT(t, filepath.Dir(dir), "clone", "-q", up, dir)
	return &modeRepo{t: t, upstream: up, dir: dir}
}

// push commits path=content upstream on main; returns the commit.
func (m *modeRepo) push(path, content string) string {
	m.t.Helper()
	os.WriteFile(filepath.Join(m.upstream, path), []byte(content), 0o644)
	gitT(m.t, m.upstream, "add", "-A")
	gitT(m.t, m.upstream, "commit", "-qm", "up "+path)
	return gitT(m.t, m.upstream, "rev-parse", "HEAD")
}

// commitLocal commits path=content in the checkout; returns the commit.
func (m *modeRepo) commitLocal(path, content string) string {
	m.t.Helper()
	m.write(path, content)
	gitT(m.t, m.dir, "add", path)
	gitT(m.t, m.dir, "commit", "-qm", "local "+path)
	return m.head()
}

func (m *modeRepo) write(path, content string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.dir, path), []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *modeRepo) read(path string) string {
	b, err := os.ReadFile(filepath.Join(m.dir, path))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (m *modeRepo) head() string { return gitT(m.t, m.dir, "rev-parse", "HEAD") }

// ref is a ref's commit in the checkout, "" when it does not exist.
func (m *modeRepo) ref(name string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", name)
	cmd.Dir = m.dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// sync bundles upstream's main as refs/lux/sync (the runner's part) and
// runs the shim's sync on the checkout in mode.
func (m *modeRepo) sync(mode string) proto.SyncResult {
	m.t.Helper()
	commit := gitT(m.t, m.upstream, "rev-parse", "main")
	gitT(m.t, m.upstream, "update-ref", "refs/lux/sync", commit)
	bundle := filepath.Join(m.t.TempDir(), "r.bundle")
	gitT(m.t, m.upstream, "bundle", "create", "-q", bundle, "refs/lux/sync")
	got := SyncRepos(context.Background(), proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: m.dir, Ref: "main", Mode: mode,
		Commit: commit, Branch: "main", Bundle: bundle}}})
	if len(got) != 1 {
		m.t.Fatalf("results: %+v", got)
	}
	return got[0]
}

// tree is every file of the working tree but .git, with its bytes.
func (m *modeRepo) tree() map[string]string {
	m.t.Helper()
	out := map[string]string{}
	err := filepath.Walk(m.dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() && fi.Name() == ".git" {
			return filepath.SkipDir
		}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			rel, _ := filepath.Rel(m.dir, p)
			out[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return out
}

func (m *modeRepo) index() string { return gitT(m.t, m.dir, "ls-files", "-s") }

func counts(r proto.SyncResult) (int, int) {
	if r.Ahead == nil || r.Behind == nil {
		return -1, -1
	}
	return *r.Ahead, *r.Behind
}

// snapshot is what a sync that does not move must leave as it was: HEAD,
// its branch, the index, refs/lux/pre-sync and the working tree's bytes.
type snapshot struct {
	head, branch, index, preSync string
	tree                         map[string]string
}

func (m *modeRepo) snap() snapshot {
	return snapshot{m.head(), gitT(m.t, m.dir, "symbolic-ref", "-q", "HEAD"), m.index(), m.ref("refs/lux/pre-sync"), m.tree()}
}

func (m *modeRepo) unchanged(before snapshot) {
	m.t.Helper()
	after := m.snap()
	if after.head != before.head || after.branch != before.branch || after.index != before.index || after.preSync != before.preSync {
		m.t.Fatalf("moved: before %+v\nafter %+v", before, after)
	}
	if len(after.tree) != len(before.tree) {
		m.t.Fatalf("tree: before %v, after %v", before.tree, after.tree)
	}
	for p, b := range before.tree {
		if after.tree[p] != b {
			m.t.Fatalf("%s: %q, was %q", p, after.tree[p], b)
		}
	}
}

func TestSyncFastForward(t *testing.T) {
	t.Run("clean ancestor: moves", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		m.write("untracked.txt", "mine\n")
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "fast-forward" || res.Mode != proto.SyncFastForward || res.From != base || res.To != c2 ||
			res.Dirty || res.Diverged || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		if m.head() != c2 || m.ref("refs/heads/main") != c2 || m.read("a.txt") != "two\n" || m.read("untracked.txt") != "mine\n" {
			t.Fatalf("after: head %s, a.txt %q", m.head(), m.read("a.txt"))
		}
		if m.ref("refs/remotes/lux/main") != c2 || m.ref("refs/lux/sync") != c2 || m.ref("refs/lux/pre-sync") != "" {
			t.Fatalf("refs: lux/main %q, sync %q, pre-sync %q", m.ref("refs/remotes/lux/main"), m.ref("refs/lux/sync"), m.ref("refs/lux/pre-sync"))
		}
	})
	t.Run("dirty ancestor: kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		m.write("untracked.txt", "mine\n")
		m.write("staged.txt", "staged\n")
		gitT(t, m.dir, "add", "staged.txt")
		before := m.snap()
		c2 := m.push("b.txt", "bee\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Dirty || res.Diverged || res.To != c2 || res.Saved != "" || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.read("a.txt") != "local edit\n" || m.read("untracked.txt") != "mine\n" || m.read("staged.txt") != "staged\n" {
			t.Fatal("files changed")
		}
		if m.ref("refs/remotes/lux/main") != c2 || m.ref("refs/lux/sync") != c2 {
			t.Fatalf("refs/remotes/lux/main %q, want %s", m.ref("refs/remotes/lux/main"), c2)
		}
	})
	t.Run("dirty at the commit: kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Dirty || a != 0 || b != 0 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
	})
	t.Run("local commits: ahead", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		local := m.commitLocal("c.txt", "mine\n")
		m.commitLocal("d.txt", "mine too\n")
		mine := m.head()
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "ahead" || res.Dirty || res.Diverged || res.To != base || a != 2 || b != 0 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.head() != mine || gitT(t, m.dir, "rev-parse", mine+"~1") != local || m.ref("refs/remotes/lux/main") != base {
			t.Fatal("history changed")
		}
	})
	t.Run("diverged: kept", func(t *testing.T) {
		m := newModeRepo(t)
		local := m.commitLocal("c.txt", "mine\n")
		m.write("untracked.txt", "u\n")
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Diverged || res.Dirty || res.From != local || res.To != c2 || a != 1 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.ref("refs/remotes/lux/main") != c2 {
			t.Fatal("refs/remotes/lux/main not set")
		}
		// The workload can merge it itself.
		gitT(t, m.dir, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-edit", "lux/main")
		if m.read("a.txt") != "two\n" || m.read("c.txt") != "mine\n" {
			t.Fatal("merge of lux/main")
		}
	})
	t.Run("up to date", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "up-to-date" || res.To != base || a != 0 || b != 0 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/remotes/lux/main") != base {
			t.Fatal("refs/remotes/lux/main not set")
		}
	})
	t.Run("ignored file the commit adds: failed, its bytes kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write(".git/info/exclude", "ignored.db\n")
		m.write("ignored.db", "LOCAL WORK\n")
		before := m.snap()
		c2 := m.push("ignored.db", "UPSTREAM\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "failed" && res.Status != "kept" {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.read("ignored.db") != "LOCAL WORK\n" || m.ref("refs/heads/main") != before.head || m.ref("refs/remotes/lux/main") != c2 {
			t.Fatalf("ignored.db %q, main %s", m.read("ignored.db"), m.ref("refs/heads/main"))
		}
	})
	t.Run("HEAD on another branch: main's own commits kept", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		m.commitLocal("c.txt", "on main\n")
		mainTip := m.head()
		gitT(t, m.dir, "checkout", "-q", "-b", "side", base)
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "kept" || res.To != c2 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/heads/main") != mainTip {
			t.Fatal("main moved")
		}
	})
}

func TestSyncFetch(t *testing.T) {
	for _, c := range []struct {
		name          string
		prep          func(m *modeRepo)
		ahead, behind int
	}{
		{"behind, clean", func(m *modeRepo) { m.push("a.txt", "two\n"); m.push("b.txt", "bee\n") }, 0, 2},
		{"behind, dirty", func(m *modeRepo) { m.write("a.txt", "edit\n"); m.push("b.txt", "bee\n") }, 0, 1},
		{"ahead", func(m *modeRepo) { m.commitLocal("c.txt", "c\n") }, 1, 0},
		{"diverged", func(m *modeRepo) {
			m.commitLocal("c.txt", "c\n")
			m.commitLocal("d.txt", "d\n")
			m.push("a.txt", "two\n")
		}, 2, 1},
		{"at the commit", func(*modeRepo) {}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newModeRepo(t)
			c.prep(m)
			m.write("untracked.txt", "u\n")
			before := m.snap()
			res := m.sync(proto.SyncFetch)
			want := gitT(t, m.upstream, "rev-parse", "main")
			if a, b := counts(res); res.Status != "fetched" || res.Mode != proto.SyncFetch || res.To != want || a != c.ahead || b != c.behind {
				t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
			}
			m.unchanged(before)
			if m.ref("refs/remotes/lux/main") != want || m.ref("refs/lux/sync") != want {
				t.Fatal("refs not set")
			}
		})
	}
}

// A detached target (a tag or sha: no Branch) sets no remote-tracking ref.
func TestSyncFastForwardDetached(t *testing.T) {
	m := newModeRepo(t)
	c2 := m.push("a.txt", "two\n")
	gitT(t, m.upstream, "update-ref", "refs/lux/sync", c2)
	bundle := filepath.Join(t.TempDir(), "r.bundle")
	gitT(t, m.upstream, "bundle", "create", "-q", bundle, "refs/lux/sync")
	got := SyncRepos(context.Background(), proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: m.dir, Ref: c2, Mode: proto.SyncFastForward,
		Commit: c2, Bundle: bundle}}})
	if got[0].Status != "fast-forward" || m.head() != c2 || m.ref("refs/remotes/lux/main") != "" ||
		gitT(t, m.dir, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatalf("%+v", got[0])
	}
}

// raceGit puts a git ahead of the real one on PATH that, the first time
// it is asked to move a branch or HEAD (checkout, merge, update-ref of a
// refs/heads/ ref), first makes a real commit: on HEAD in the checkout,
// or, with onRef, on that ref. Returns the file the commit's id lands in.
func raceGit(t *testing.T, onRef string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "raced")
	script := `#!/bin/sh
REAL='` + real + `'
hit=; upd=
for a in "$@"; do
	case "$a" in
	checkout|merge) hit=1 ;;
	update-ref) upd=1 ;;
	refs/heads/*) [ -n "$upd" ] && hit=1 ;;
	esac
done
if [ -n "$hit" ] && mkdir '` + out + `.once' 2>/dev/null; then
	id="-c user.name=t -c user.email=t@t"
	if [ -n '` + onRef + `' ]; then
		c=$("$REAL" $id commit-tree -p '` + onRef + `' -m raced "$("$REAL" rev-parse '` + onRef + `^{tree}')")
		"$REAL" update-ref '` + onRef + `' "$c"
	else
		echo raced > local-race
		"$REAL" add local-race
		"$REAL" $id commit -qm raced
		c=$("$REAL" rev-parse HEAD)
	fi
	echo "$c" > '` + out + `'
fi
exec "$REAL" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}

// A commit the workload makes while a fast-forward runs, between its
// checks and its move, is never reset away.
func TestSyncFastForwardRacedCommit(t *testing.T) {
	t.Run("HEAD on the branch", func(t *testing.T) {
		m := newModeRepo(t)
		c2 := m.push("a.txt", "two\n")
		raced := raceGit(t, "")
		res := m.sync(proto.SyncFastForward)
		b, err := os.ReadFile(raced)
		if err != nil {
			t.Fatalf("no commit raced: %v (%+v)", err, res)
		}
		local := strings.TrimSpace(string(b))
		if res.Status == "fast-forward" || m.ref("refs/heads/main") != local || m.head() != local || m.read("local-race") != "raced\n" {
			t.Fatalf("%+v: main %s, raced %s", res, m.ref("refs/heads/main"), local)
		}
		if m.ref("refs/remotes/lux/main") != c2 {
			t.Fatal("refs/remotes/lux/main not set")
		}
	})
	t.Run("HEAD on another branch", func(t *testing.T) {
		m := newModeRepo(t)
		gitT(t, m.dir, "checkout", "-q", "-b", "side")
		m.push("a.txt", "two\n")
		raced := raceGit(t, "refs/heads/main")
		res := m.sync(proto.SyncFastForward)
		b, err := os.ReadFile(raced)
		if err != nil {
			t.Fatalf("no commit raced: %v (%+v)", err, res)
		}
		local := strings.TrimSpace(string(b))
		if res.Status == "fast-forward" || m.ref("refs/heads/main") != local || gitT(t, m.dir, "symbolic-ref", "HEAD") != "refs/heads/side" {
			t.Fatalf("%+v: main %s, raced %s", res, m.ref("refs/heads/main"), local)
		}
	})
}

// An unknown mode fails, the checkout untouched.
func TestSyncUnknownMode(t *testing.T) {
	m := newModeRepo(t)
	m.push("a.txt", "two\n")
	before := m.snap()
	res := m.sync("rebase")
	if res.Status != "failed" || !strings.Contains(res.Error, `unknown sync mode "rebase"`) {
		t.Fatalf("%+v", res)
	}
	m.unchanged(before)
}

// Mode move, explicitly or by default, still resets a dirty checkout.
func TestSyncMoveStillResets(t *testing.T) {
	for _, mode := range []string{"", proto.SyncMove} {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		c2 := m.push("b.txt", "bee\n")
		res := m.sync(mode)
		if res.Status != "reset" || res.Mode != proto.SyncMove || res.Ahead != nil || res.Saved != "refs/lux/pre-sync" || m.head() != c2 ||
			m.read("a.txt") != "one\n" || m.ref("refs/remotes/lux/main") != "" {
			t.Fatalf("mode %q: %+v", mode, res)
		}
	}
}
