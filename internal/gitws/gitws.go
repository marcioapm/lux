// Package gitws materializes a Run's git repositories and pushes its work,
// on the host. Git credentials are used here, by the runner, and never
// enter the container: the checkout's remote is the plain URL, and a push
// authenticates through a one-off header that is never written to disk.
//
// Every repository is fetched into a host-local mirror first (shared by
// every Run on the host, updated on use), then cloned from the mirror into
// the Run's workspace volume. The clone is a plain, self-contained
// repository: it lives on a state volume and must survive a move to a host
// without that mirror.
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
	"strings"
	"sync"
	"time"
)

// Repo is one repository to materialize.
type Repo struct {
	Name  string
	URL   string
	Ref   string // branch, tag or sha
	Path  string // inside the container; for the manifest only
	Token string // credential for fetch and push; "" for public repos
}

// Result is what was checked out.
type Result struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Branch string `json:"branch,omitempty"` // the branch checked out, if Ref is one
	Base   string `json:"base"`             // the commit checked out
}

type Manager struct {
	mirrors string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

func New(dataDir string) *Manager {
	return &Manager{mirrors: filepath.Join(dataDir, "mirrors"), locks: map[string]*sync.Mutex{}}
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
	entries, _ := os.ReadDir(m.mirrors)
	var out []string
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(m.mirrors, e.Name(), "lux-url")); err == nil {
			out = append(out, strings.TrimSpace(string(b)))
		}
	}
	return out
}

func (m *Manager) mirrorPath(u string) string {
	h := sha256.Sum256([]byte(u))
	return filepath.Join(m.mirrors, hex.EncodeToString(h[:12])+".git")
}

// git runs git with a token as a one-off Authorization header: it is in
// this process's environment for this command only, never in a config
// file or a URL that could be logged or left behind.
func git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "LC_ALL=C",
		// The runner (root) works in checkouts the workload user owns; git
		// would refuse them as "dubious ownership". Trusted for this
		// command only: nothing is written to any config.
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*")
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1=Authorization: Basic "+auth)
	} else {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_KEY_1=core.askPass", "GIT_CONFIG_VALUE_1=/bin/false")
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

// mirror makes sure the host has an up-to-date mirror of the repository.
func (m *Manager) mirror(ctx context.Context, r Repo) (string, error) {
	path := m.mirrorPath(r.URL)
	defer m.lock(path)()
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
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
	// The URL is kept for Mirrors(), without any credential (tokens never
	// go in URLs here).
	if err := os.WriteFile(filepath.Join(tmp, "lux-url"), []byte(r.URL), 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// Materialize clones a repository into dir (on the Run's workspace
// volume), at r.Ref. An existing checkout is left alone: it holds the
// Run's work, which is the point of a state volume.
func (m *Manager) Materialize(ctx context.Context, r Repo, dir string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return m.describe(ctx, r, dir)
	}
	mirror, err := m.mirror(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
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
		head, err := git(ctx, mirror, "", "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return nil, err
		}
		ref = head
	}
	// A branch is checked out as a local branch tracking origin; a tag or
	// sha detached.
	if _, err := git(ctx, dir, "", "rev-parse", "--verify", "-q", "refs/remotes/origin/"+ref); err == nil {
		if _, err := git(ctx, dir, "", "checkout", "--quiet", "-B", ref, "origin/"+ref); err != nil {
			return nil, err
		}
	} else if _, err := git(ctx, dir, "", "checkout", "--quiet", "--detach", ref); err != nil {
		return nil, fmt.Errorf("ref %q not found in %s", ref, r.URL)
	}
	res, err := m.describe(ctx, r, dir)
	if err != nil {
		return nil, err
	}
	// Remember where this Run started, for push leases and diffs.
	_, _ = git(ctx, dir, "", "update-ref", "refs/lux/base", res.Base)
	return res, nil
}

func (m *Manager) describe(ctx context.Context, r Repo, dir string) (*Result, error) {
	base, err := git(ctx, dir, "", "rev-parse", "--verify", "-q", "refs/lux/base")
	if err != nil {
		if base, err = git(ctx, dir, "", "rev-parse", "HEAD"); err != nil {
			return nil, err
		}
	}
	branch, _ := git(ctx, dir, "", "symbolic-ref", "--short", "-q", "HEAD")
	return &Result{Name: r.Name, URL: r.URL, Ref: r.Ref, Branch: branch, Base: base}, nil
}

// PushResult says what a push did.
type PushResult struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Status string `json:"status"` // pushed | up-to-date | rejected | failed
	Error  string `json:"error,omitempty"`
}

// Push pushes the checkout's HEAD to branch on the real remote.
//
// The lease is explicit: replace the branch only if it still points where
// this Run left it the last time it pushed (or, the first time, only if it
// does not exist yet). A bare --force-with-lease cannot work here: pushing
// to a URL has no remote-tracking ref for git to compare against.
func (m *Manager) Push(ctx context.Context, r Repo, dir, branch string) PushResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := PushResult{Repo: r.Name, Branch: branch}
	head, err := git(ctx, dir, "", "rev-parse", "HEAD")
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	res.Commit = head
	pushed, _ := git(ctx, dir, "", "rev-parse", "--verify", "-q", "refs/lux/pushed/"+branch)
	if pushed == head {
		res.Status = "up-to-date"
		return res
	}
	ref := "refs/heads/" + branch
	lease := "--force-with-lease=" + ref + ":" + pushed
	out, err := git(ctx, dir, r.Token, "push", "--porcelain", lease, r.URL, "HEAD:"+ref)
	if err != nil {
		res.Status, res.Error = "failed", out
		if strings.Contains(out, "stale info") || strings.Contains(out, "rejected") {
			res.Status = "rejected"
			res.Error = "the branch moved since this Run last pushed it: not overwritten"
		}
		return res
	}
	_, _ = git(ctx, dir, "", "update-ref", "refs/lux/pushed/"+branch, head)
	res.Status = "pushed"
	return res
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
