package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/spec"
)

// materializeRepos clones the Run's repositories into its workspace volume,
// on the host, before the container starts. A resumed Run's checkouts are
// already on its restored volume and are left as they are: they hold its
// work. Credentials are the runner's; the container never sees them.
func (p *placement) materializeRepos(ctx context.Context, sp spec.RunSpec, image string) error {
	if sp.Git == nil || len(sp.Git.Repositories) == 0 {
		return nil
	}
	uid, gid := p.imageUser(ctx, sp, image)
	for _, r := range sp.Git.Repositories {
		dir, err := p.hostPath(ctx, sp, r.Path)
		if err != nil {
			return err
		}
		res, err := p.r.git.Materialize(ctx, p.gitRepo(r), dir)
		if err != nil {
			return fmt.Errorf("repository %s: %w", r.Name, err)
		}
		// Written by the runner as root: hand the checkout to the workload
		// user (the volume is idmapped, so container uids are host uids).
		if err := chownTree(dir, uid, gid); err != nil {
			return err
		}
		p.event(ctx, "git.checkout", map[string]any{"repo": res.Name, "url": gitws.Scrub(res.URL),
			"ref": res.Ref, "branch": res.Branch, "base": res.Base})
	}
	return nil
}

func (p *placement) gitRepo(r spec.Repository) gitws.Repo {
	return gitws.Repo{Name: r.Name, URL: r.URL, Ref: r.Ref, Path: r.Path, Token: p.assign.Secrets[r.Credential]}
}

// hostPath is where a container path on one of the Run's volumes is on the
// host.
func (p *placement) hostPath(ctx context.Context, sp spec.RunSpec, path string) (string, error) {
	for _, v := range p.state.Volumes {
		if path == v.Path || strings.HasPrefix(path, v.Path+"/") {
			mp, err := p.r.mountpoint(ctx, v.Volume)
			if err != nil {
				return "", err
			}
			return filepath.Join(mp, strings.TrimPrefix(path, v.Path)), nil
		}
	}
	return "", fmt.Errorf("%s is not on a volume", path)
}

// imageUser is the uid and gid the workload runs as: the spec's user, or
// the image's, resolved against the image's /etc/passwd.
func (p *placement) imageUser(ctx context.Context, sp spec.RunSpec, image string) (int, int) {
	user := sp.Workload.User
	if user == "" {
		out, _ := p.r.pm.Run(ctx, "image", "inspect", "--format", "{{.Config.User}}", image)
		user = strings.TrimSpace(string(out))
	}
	name, group, _ := strings.Cut(user, ":")
	if name == "" || name == "root" || name == "0" {
		return 0, 0
	}
	uid, uerr := strconv.Atoi(name)
	gid := uid
	if uerr != nil {
		out, err := p.r.pm.Run(ctx, "run", "--rm", "--entrypoint", "", "--network=none", image, "cat", "/etc/passwd")
		if err != nil {
			return 0, 0
		}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Split(l, ":")
			if len(f) >= 4 && f[0] == name {
				uid, _ = strconv.Atoi(f[2])
				gid, _ = strconv.Atoi(f[3])
			}
		}
	}
	if g, err := strconv.Atoi(group); err == nil {
		gid = g
	}
	return uid, gid
}

func chownTree(root string, uid, gid int) error {
	if uid == 0 && gid == 0 {
		return nil
	}
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// push pushes every repository's HEAD to the spec's push branch, with the
// runner's credentials, and reports the outcome as an event.
func (p *placement) push(ctx context.Context, requestID, _ string) {
	sp := p.assign.Spec
	if sp.Git == nil || sp.Git.Push == nil {
		return
	}
	for _, r := range sp.Git.Repositories {
		dir, err := p.hostPath(ctx, sp, r.Path)
		res := gitws.PushResult{Repo: r.Name, Branch: sp.Git.Push.Branch, Status: "failed"}
		if err == nil {
			res = p.r.git.Push(ctx, p.gitRepo(r), dir, sp.Git.Push.Branch)
		} else {
			res.Error = err.Error()
		}
		p.event(ctx, "git.push", map[string]any{"requestId": requestID, "repo": res.Repo, "branch": res.Branch,
			"commit": res.Commit, "status": res.Status, "error": res.Error})
	}
}
