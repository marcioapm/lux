package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Syncing a Run's checkouts to new commits (a resume's sync, or
// MsgSync while it runs). The runner fetches each ref through the host's
// mirror with its credential and writes a bundle of it on the Run's
// runtime volume; `lux-shim sync`, as the workload user in the container,
// moves the checkout (shim/sync.go has the rule). The runner never runs
// git in the checkout, and no credential enters the container. Each
// repository's outcome is a git.sync event.

const syncDir = "sync" // on the runtime volume: /.lux/run/sync

// prepareSync fetches and bundles each ref, and returns what the shim is
// to do. A repository that cannot be prepared is reported failed here and
// left out: the Run goes on.
func (p *placement) prepareSync(ctx context.Context, sp spec.RunSpec, refs []proto.SyncRef, requestID string) *proto.SyncArgs {
	if len(refs) == 0 {
		return nil
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err == nil {
		dir := filepath.Join(rt, syncDir)
		os.RemoveAll(dir)
		err = os.Mkdir(dir, 0o755)
	}
	args := &proto.SyncArgs{}
	for _, ref := range refs {
		res := proto.SyncResult{Repo: ref.Repo, Ref: ref.Ref, Status: "failed"}
		var repo *spec.Repository
		if sp.Git != nil {
			for i := range sp.Git.Repositories {
				if sp.Git.Repositories[i].Name == ref.Repo {
					repo = &sp.Git.Repositories[i]
				}
			}
		}
		switch {
		case err != nil:
			res.Error = "runtime volume: " + err.Error()
		case repo == nil:
			res.Error = "no repository " + ref.Repo + " in the spec"
		default:
			r := p.gitRepo(*repo)
			r.Ref = ref.Ref
			name := ref.Repo + ".bundle"
			t, berr := p.r.git.SyncBundle(ctx, r, filepath.Join(rt, syncDir, name))
			if berr == nil {
				args.Repos = append(args.Repos, proto.SyncRepo{Name: ref.Repo, Path: repo.Path, Ref: ref.Ref, Commit: t.Commit,
					Branch: t.Branch, Bundle: proto.ShimRunDir + "/" + syncDir + "/" + name})
				continue
			}
			res.Error = strings.ReplaceAll(berr.Error(), repo.URL, gitws.Scrub(repo.URL))
		}
		p.reportSync(ctx, res, requestID)
	}
	if len(args.Repos) == 0 {
		return nil
	}
	return args
}

// reportSync sends a repository's git.sync event, and keeps a moved
// checkout's commit as the base of its live diffs.
func (p *placement) reportSync(ctx context.Context, res proto.SyncResult, requestID string) {
	if (res.Status == "fast-forward" || res.Status == "reset") && res.To != "" {
		p.setGitBase(res.Repo, res.To)
	}
	var d map[string]any
	b, _ := json.Marshal(res)
	_ = json.Unmarshal(b, &d)
	if requestID != "" {
		d["requestId"] = requestID
	}
	_ = p.reportRetrying(ctx, proto.RunEvent{Type: proto.EvGitSync, Data: d})
}

// syncRunning is MsgSync: move a running placement's checkouts, then tell
// luxd (sync.done) whether any moved, so servers with afterSync restart.
func (p *placement) syncRunning(ctx context.Context, req proto.Sync) {
	p.mu.Lock()
	running := p.phase == "running"
	user := ""
	var sp spec.RunSpec
	if p.state != nil {
		user = p.state.User
		if p.state.Spec != nil {
			sp = *p.state.Spec
		}
	}
	p.mu.Unlock()
	done := map[string]any{"requestId": req.RequestID, "changed": false}
	// Reported with ctx, never a context cancelled meanwhile: a write
	// cancelled half-way closes the connection to luxd.
	defer func() { _ = p.reportRetrying(ctx, proto.RunEvent{Type: proto.EvSyncDone, Data: done}) }()
	if !running || user == "" {
		for _, r := range req.Repos {
			p.reportSync(ctx, proto.SyncResult{Repo: r.Repo, Ref: r.Ref, Status: "failed", Error: "the Run is not running"}, req.RequestID)
		}
		return
	}
	args := p.prepareSync(ctx, sp, req.Repos, req.RequestID)
	if args == nil {
		return
	}
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := p.r.pm.Run(execCtx, "exec", "--user", user, "--workdir", "/", containerName(p.runID),
		proto.ShimBinary, "sync", string(proto.Marshal(args)))
	var results []proto.SyncResult
	if err == nil {
		err = json.Unmarshal(out, &results)
	}
	if err != nil {
		for _, r := range args.Repos {
			p.reportSync(ctx, proto.SyncResult{Repo: r.Name, Ref: r.Ref, To: r.Commit, Status: "failed",
				Error: fmt.Sprintf("lux-shim sync: %v %s", err, strings.TrimSpace(string(out[:min(len(out), 300)])))}, req.RequestID)
		}
		return
	}
	for _, res := range results {
		p.reportSync(ctx, res, req.RequestID)
		if res.Status == "fast-forward" || res.Status == "reset" {
			done["changed"] = true
		}
	}
}

// onSyncRecord reports the shim's sync before init (a lux.sync event in
// the output file) as git.sync events.
func (p *placement) onSyncRecord(ctx context.Context, data json.RawMessage) {
	var d struct {
		Results []proto.SyncResult `json:"results"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	for _, res := range d.Results {
		p.reportSync(ctx, res, "")
	}
}
