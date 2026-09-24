// Package gitws materializes a Run's git repositories and pushes its work,
// on the host. Git credentials are used here, by the runner, and never
// enter the container: the checkout's remote is the plain URL, and fetches
// and pushes authenticate through a one-off header that is never written
// to disk.
//
// The runner never runs git inside a checkout once the workload has had
// it: the workload controls its .git (config, hooks), and git run there as
// root with the token would run the workload's hooks as root and follow
// its config (insteadOf, proxies) with the token. So a push takes a bundle
// the workload produced (as itself, inside its container), unpacks it into
// a runner-owned bare repository, and pushes from there.
package gitws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Repo is one repository to materialize or push to.
type Repo struct {
	Name  string
	URL   string
	Ref   string // branch, tag or sha
	Token string // credential for fetch and push; "" for public repos
}

// Result is what was checked out.
type Result struct {
	Name   string `json:"name"`
	Cloned bool   `json:"-"`                // false when the checkout already existed (a resume)
	Branch string `json:"branch,omitempty"` // the branch checked out, if Ref is one
	Base   string `json:"base"`             // the commit checked out
}

type Manager struct {
	mirrors string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	known   map[string]bool // mirrored URLs
}

func New(dataDir string) *Manager {
	m := &Manager{mirrors: filepath.Join(dataDir, "mirrors"), locks: map[string]*sync.Mutex{}, known: map[string]bool{}}
	entries, _ := os.ReadDir(m.mirrors)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(m.mirrors, e.Name(), "lux-url")); err == nil {
			m.known[strings.TrimSpace(string(b))] = true
		}
	}
	return m
}

func (m *Manager) lock(key string) func() {
	m.mu.Lock()
	l := m.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Mirrors lists the URLs this host has mirrors for (for scheduling).
func (m *Manager) Mirrors() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.known))
	for u := range m.known {
		out = append(out, u)
	}
	return out
}

func (m *Manager) mirrorPath(u string) string {
	h := sha256.Sum256([]byte(u))
	return filepath.Join(m.mirrors, hex.EncodeToString(h[:12])+".git")
}

// git runs git on a repository the runner owns, with a token as a one-off
// Authorization header: in this process's environment for this command
// only, never in a config file or a URL. Hooks are off: nothing in a
// repository runs code as the runner.
func git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[REDACTED]")
		}
		return msg, fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// mirror makes sure the host has a mirror of the repository holding ref.
// A branch is fetched every time (it moves); a full sha already in the
// mirror is not.
func (m *Manager) mirror(ctx context.Context, r Repo) (string, error) {
	path := m.mirrorPath(r.URL)
	defer m.lock(path)()
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if shaRe.MatchString(r.Ref) {
			if _, err := git(ctx, path, "", "cat-file", "-e", r.Ref+"^{commit}"); err == nil {
				return path, nil
			}
		}
		if _, err := git(ctx, path, r.Token, "fetch", "--prune", "--tags", r.URL, "+refs/heads/*:refs/heads/*"); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(m.mirrors, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	os.RemoveAll(tmp)
	if _, err := git(ctx, m.mirrors, r.Token, "clone", "--mirror", "--quiet", r.URL, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	// The URL is kept for Mirrors(), never with a credential.
	if err := os.WriteFile(filepath.Join(tmp, "lux-url"), []byte(r.URL), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.known[r.URL] = true
	m.mu.Unlock()
	return path, nil
}

// Materialize clones a repository into dir (on the Run's workspace
// volume), at r.Ref, and returns what it checked out. An existing checkout
// is left exactly as it is (it holds the Run's work) and not looked into:
// the workload controls it now.
func (m *Manager) Materialize(ctx context.Context, r Repo, dir string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return &Result{Name: r.Name}, nil
	}
	mirror, err := m.mirror(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	// Built beside dir and renamed into place when complete: a clone that
	// fails half-way leaves nothing a later start would take for a
	// finished checkout.
	final := dir
	dir = filepath.Join(filepath.Dir(final), ".lux-clone-"+filepath.Base(final))
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	// --no-hardlinks: the clone must stand alone on the volume.
	if _, err := git(ctx, filepath.Dir(dir), "", "clone", "--quiet", "--no-hardlinks", "--no-checkout", mirror, dir); err != nil {
		return nil, err
	}
	// The remote the workload sees is the real URL, with no credential.
	if _, err := git(ctx, dir, "", "remote", "set-url", "origin", r.URL); err != nil {
		return nil, err
	}
	ref := r.Ref
	if ref == "" || ref == "HEAD" {
		// The remote's default branch, as the mirror records it.
		if ref, err = git(ctx, mirror, "", "symbolic-ref", "--short", "HEAD"); err != nil {
			return nil, err
		}
	}
	res := &Result{Name: r.Name, Cloned: true}
	// A branch is checked out as a local branch tracking origin; a tag or
	// sha detached.
	if _, err := git(ctx, dir, "", "rev-parse", "--verify", "-q", "refs/remotes/origin/"+ref); err == nil {
		if _, err := git(ctx, dir, "", "checkout", "--quiet", "-B", ref, "origin/"+ref); err != nil {
			return nil, err
		}
		res.Branch = ref
	} else if _, err := git(ctx, dir, "", "checkout", "--quiet", "--detach", ref); err != nil {
		return nil, fmt.Errorf("ref %q not found in %s", ref, Scrub(r.URL))
	}
	if res.Base, err = git(ctx, dir, "", "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	if err := os.Rename(dir, final); err != nil {
		return nil, err
	}
	return res, nil
}

// PushResult says what a push did.
type PushResult struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit,omitempty"`
	Status string `json:"status"` // pushed | up-to-date | rejected | failed | skipped
	Error  string `json:"error,omitempty"`
}

// Push pushes the commit in bundle (made by the workload from its HEAD) to
// branch on the real remote, from a runner-owned repository.
//
// The lease is explicit: replace the branch only if it is still at lease,
// the commit this Run last pushed there ("" the first time: only if the
// branch does not exist). A bare --force-with-lease cannot work: pushing to
// a URL has no remote-tracking ref to compare against.
func (m *Manager) Push(ctx context.Context, r Repo, bundle, branch, lease string) PushResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := PushResult{Repo: r.Name, Branch: branch, Status: "failed"}
	mirror, err := m.mirror(ctx, Repo{Name: r.Name, URL: r.URL, Ref: branch, Token: r.Token})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// A private scratch repository sharing the mirror's objects: the
	// bundle's commits land here, never in the shared mirror.
	work, err := os.MkdirTemp(m.mirrors, "push-")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer os.RemoveAll(work)
	if _, err := git(ctx, work, "", "init", "--quiet", "--bare"); err != nil {
		res.Error = err.Error()
		return res
	}
	alt := filepath.Join(work, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(mirror, "objects")+"\n"), 0o600); err != nil {
		res.Error = err.Error()
		return res
	}
	if _, err := git(ctx, work, "", "fetch", "--quiet", bundle, "+HEAD:refs/lux/push"); err != nil {
		res.Error = "the workload's commit could not be read: " + err.Error()
		return res
	}
	head, err := git(ctx, work, "", "rev-parse", "refs/lux/push")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Commit = head
	// Always asked of the remote, even when head is the lease: the lease
	// may be a caller's expectation, not where the branch is.
	ref := "refs/heads/" + branch
	out, err := git(ctx, work, r.Token, "push", "--porcelain", "--force-with-lease="+ref+":"+lease, r.URL, "refs/lux/push:"+ref)
	if err != nil {
		res.Error = out
		// Only a failed lease means the branch moved; a server refusing the
		// push (a hook, a protected branch) keeps its own message.
		if strings.Contains(out, "stale info") || strings.Contains(out, "[rejected]") && strings.Contains(out, "fetch first") {
			res.Status = "rejected"
			res.Error = "the branch moved since this Run last pushed it: not overwritten"
		}
		return res
	}
	res.Status = "pushed"
	if pushUpToDate(out) {
		res.Status = "up-to-date"
	}
	return res
}

// pushUpToDate: the --porcelain output of a push that changed nothing (the
// remote already had the commit) flags its ref line "=".
func pushUpToDate(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "=\t") || strings.Contains(l, "[up to date]") {
			return true
		}
	}
	return false
}

// Scrub removes anything credential-like a URL might carry, for display.
func Scrub(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.User == nil {
		return u
	}
	p.User = nil
	return p.String()
}
