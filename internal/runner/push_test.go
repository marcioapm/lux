package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// containerExec is a podman whose `exec --user U --workdir W NAME cmd...`
// runs cmd on the host in $ROOT/co/W, with /.lux/run in any argument read
// as the runtime volume ($ROOT/rt); git is a wrapper that appends each
// `git bundle` it runs to $ROOT/bundles. Anything else exits 125.
const containerExec = `ROOT=$(dirname "$BARE")
[ "$1" = exec ] && [ "$2" = --user ] && [ "$4" = --workdir ] || exit 125
cd "$ROOT/co$5" || exit 125
shift 6
n=$#
while [ $n -gt 0 ]; do
	a=$1; shift
	case "$a" in /.lux/run/*) a="$ROOT/rt${a#/.lux/run}" ;; esac
	set -- "$@" "$a"
	n=$((n-1))
done
PATH="$ROOT/bin:$PATH" exec "$@"
`

// pushFixture is a syncFixture whose spec pushes app and lib to lux/x,
// each a checkout at $ROOT/co/w/<name> cloned from its bare repository,
// run through containerExec.
type pushFixture struct {
	*syncFixture
	root  string
	bares map[string]string
}

func newPushFixture(t *testing.T, script string) *pushFixture {
	t.Helper()
	f := newSyncFixture(t, script)
	root := filepath.Dir(f.bare)
	pf := &pushFixture{syncFixture: f, root: root, bares: map[string]string{"app": f.bare, "lib": filepath.Join(root, "lib.git")}}
	run(t, root, "git", "clone", "-q", "--bare", f.bare, pf.bares["lib"])
	for name, bare := range pf.bares {
		run(t, root, "git", "clone", "-q", bare, pf.checkout(name))
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	wrapper := "#!/bin/sh\n[ \"$1\" = bundle ] && echo \"$PWD\" >> '" + filepath.Join(root, "bundles") + "'\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	f.sp.Git = &spec.Git{Repositories: []spec.Repository{
		{Name: "app", URL: pf.bares["app"], Path: "/w/app"},
		{Name: "lib", URL: pf.bares["lib"], Path: "/w/lib"},
	}, Push: &spec.Push{Branch: "lux/x"}}
	f.p.assign.Spec = f.sp
	f.p.user = passwd.User{UID: os.Getuid(), GID: os.Getgid()}
	// An old container's lux-shim: the refusal must not depend on it.
	f.p.state.ShimSyncModes = false
	return pf
}

func (pf *pushFixture) checkout(name string) string { return filepath.Join(pf.root, "co", "w", name) }

// bundled is the checkouts a git bundle ran in.
func (pf *pushFixture) bundled() string {
	b, _ := os.ReadFile(filepath.Join(pf.root, "bundles"))
	return string(b)
}

// branch is lux/x in name's bare repository, "" when absent.
func (pf *pushFixture) branch(name string) string {
	out, _ := exec.Command("git", "-C", pf.bares[name], "rev-parse", "--verify", "-q", "refs/heads/lux/x").Output()
	return strings.TrimSpace(string(out))
}

// pushed runs a push and returns its git.push results by repository.
func (pf *pushFixture) pushed(t *testing.T) map[string]map[string]any {
	t.Helper()
	pf.p.push(context.Background(), proto.Push{RequestID: "rq"})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		pf.mu.Lock()
		evs := pf.pushes
		pf.mu.Unlock()
		if len(evs) == 0 {
			continue
		}
		out := map[string]map[string]any{}
		for _, x := range evs[0]["results"].([]any) {
			r := x.(map[string]any)
			out[r["repo"].(string)] = r
		}
		return out
	}
	t.Fatal("no git.push event")
	return nil
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkout with a rebase in progress is refused, never bundled, and
// its push branch is not created; another repository pushes as normal.
func TestPushRefusesAnOperationInProgress(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app, lib := pf.checkout("app"), pf.checkout("lib")
	// app: a local commit rebased onto a conflicting one, stopped.
	run(t, app, "git", "checkout", "-q", "-b", "mine")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("mine\n"), 0o644)
	run(t, app, "git", "commit", "-qam", "mine")
	run(t, app, "git", "checkout", "-q", "main")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("theirs\n"), 0o644)
	run(t, app, "git", "commit", "-qam", "theirs")
	run(t, app, "git", "checkout", "-q", "mine")
	if out, err := exec.Command("git", "-C", app, "rebase", "main").CombinedOutput(); err == nil {
		t.Fatalf("the rebase did not stop: %s", out)
	}
	os.WriteFile(filepath.Join(lib, "l.txt"), []byte("lib\n"), 0o644)
	run(t, lib, "git", "add", "l.txt")
	run(t, lib, "git", "commit", "-qm", "lib")
	libHead := run(t, lib, "git", "rev-parse", "HEAD")

	res := pf.pushed(t)
	a := res["app"]
	if a["status"] != "refused" || a["operation"] != "rebase" || a["branch"] != "lux/x" ||
		a["error"] != "a rebase is in progress in the checkout: finish or abort it, then push" || a["commit"] != nil {
		t.Fatalf("app: %v", a)
	}
	if b := pf.bundled(); strings.Contains(b, "/co/w/app") {
		t.Fatalf("app was bundled: %q", b)
	}
	if got := pf.branch("app"); got != "" {
		t.Fatalf("app's lux/x pushed at %s", got)
	}
	if l := res["lib"]; l["status"] != "pushed" || l["commit"] != libHead || l["operation"] != nil || pf.branch("lib") != libHead {
		t.Fatalf("lib: %v, lux/x %q", l, pf.branch("lib"))
	}
	if entries, _ := os.ReadDir(filepath.Join(pf.root, "rt", "push")); len(entries) != 0 {
		t.Fatalf("bundles left: %v", entries)
	}
}

// Each operation is named in the refusal.
func TestPushRefusalNamesTheOperation(t *testing.T) {
	for _, c := range []struct{ op, state string }{
		{"merge", "MERGE_HEAD"}, {"rebase", "rebase-apply"}, {"cherry-pick", "CHERRY_PICK_HEAD"},
		{"revert", "REVERT_HEAD"}, {"sequencer", "sequencer"},
	} {
		t.Run(c.op, func(t *testing.T) {
			pf := newPushFixture(t, containerExec)
			// The state git leaves, as git resolves its path.
			p := run(t, pf.checkout("app"), "git", "rev-parse", "--git-path", c.state)
			if err := os.MkdirAll(filepath.Join(pf.checkout("app"), p), 0o755); err != nil {
				t.Fatal(err)
			}
			a := pf.pushed(t)["app"]
			want := "a " + c.op + " is in progress in the checkout: finish or abort it, then push"
			if a["status"] != "refused" || a["operation"] != c.op || a["error"] != want {
				t.Fatalf("%v", a)
			}
			if strings.Contains(pf.bundled(), "/co/w/app") || pf.branch("app") != "" {
				t.Fatal("bundled or pushed")
			}
		})
	}
}

// When the check cannot run, the push fails: nothing is bundled.
func TestPushFailsClosed(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"container gone", `echo "no such container" >&2; exit 125`},
		{"no shell in the image", `[ "$7" = sh ] && { echo 'executable file "sh" not found' >&2; exit 127; }
` + containerExec},
		{"not a checkout", `rm -rf "$(dirname "$BARE")/co/w/app/.git"
` + containerExec},
	} {
		t.Run(c.name, func(t *testing.T) {
			pf := newPushFixture(t, c.script)
			a := pf.pushed(t)["app"]
			if a["status"] != "failed" || a["error"] == "" || a["operation"] != nil {
				t.Fatalf("%v", a)
			}
			if strings.Contains(pf.bundled(), "/co/w/app") || pf.branch("app") != "" {
				t.Fatal("bundled or pushed")
			}
		})
	}
}
