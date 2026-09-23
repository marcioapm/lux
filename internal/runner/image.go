package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func (p *placement) ensureImage(ctx context.Context, sp spec.RunSpec, net podman.Network) (string, error) {
	if sp.Image.Build != nil {
		return p.buildImage(ctx, sp, net)
	}
	return sp.Image.Ref, p.pullIfMissing(ctx, sp.Image.Ref)
}

func (p *placement) pullIfMissing(ctx context.Context, ref string) error {
	if p.r.pm.ImageExists(ctx, ref) {
		return nil
	}
	p.event(ctx, "image.pull", map[string]any{"ref": ref})
	return p.r.pm.Pull(ctx, ref)
}

// buildImage builds the spec's Containerfile on this host. The first build
// pins every FROM to the digest it resolves to here and reports the pinned
// Containerfile; luxd keeps it, and later placements build from it, so a
// moved tag does not change what a Run runs on. Built images are tagged by
// a hash of what they are built from, so a host builds each once.
//
// The build runs tenant code, so it is contained like the workload: its own
// user namespace, the workload's capabilities, and the Run's network, with
// its egress rules and DNS stub.
func (p *placement) buildImage(ctx context.Context, sp spec.RunSpec, net podman.Network) (string, error) {
	b := sp.Image.Build
	var prev proto.ImageResolution
	if p.assign.ImageResolved != nil {
		prev = *p.assign.ImageResolved
	}
	cf := prev.Containerfile
	var err error
	if cf == "" {
		if cf, err = p.pinFroms(ctx, b.Containerfile); err != nil {
			return "", err
		}
	}
	tag := "localhost/lux-build:" + buildKey(cf, b.Args)
	id, err := p.r.pm.ImageID(ctx, tag)
	cached := err == nil
	if !cached {
		p.event(ctx, "image.build", map[string]any{"tag": tag})
		local, err := p.localBases(ctx, cf)
		if err != nil {
			return "", err
		}
		if id, err = p.build(ctx, sp, local, tag, net); err != nil {
			return "", err
		}
	}
	// luxd keeps the first of these it sees as the Run's image resolution.
	p.event(ctx, "image.built", map[string]any{"tag": tag, "imageId": id, "cached": cached, "containerfile": cf})
	if prev.ImageID != "" && prev.ImageID != id {
		// Not a failure: state volumes do not depend on the image, only the
		// writable layer would, and that does not travel.
		p.event(ctx, "image.rebuild-differs", map[string]any{"imageId": id, "firstImageId": prev.ImageID,
			"hint": "the build is not reproducible (RUN steps that fetch or date things); the Run continues on this image"})
	}
	return tag, nil
}

// build runs podman build and returns the image id.
func (p *placement) build(ctx context.Context, sp spec.RunSpec, cf, tag string, net podman.Network) (string, error) {
	dir, err := os.MkdirTemp(filepath.Join(p.r.cfg.DataDir, "tmp"), "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	// No context yet: the build sees only its Containerfile.
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(cf), 0o600); err != nil {
		return "", err
	}
	ctxDir := filepath.Join(dir, "context")
	if err := os.Mkdir(ctxDir, 0o755); err != nil {
		return "", err
	}
	args := append(containment(), "--quiet", "--pull=never", "--isolation=oci", "--layers",
		"--timestamp=0", // no build-time dates in the image: rebuilds match when the steps do
		"--memory", fmt.Sprintf("%d", int64(sp.Resources.Memory)),
		"--cpu-period=100000", fmt.Sprintf("--cpu-quota=%d", int64(sp.Resources.CPUs*100000)),
		"--network", networkName(p.runID),
		"--label", LabelManaged+"=true",
		"-t", tag, "-f", file,
	)
	args = append(args, dnsArgs(sp, net)...)
	for _, k := range slices.Sorted(maps.Keys(sp.Image.Build.Args)) {
		args = append(args, "--build-arg", k+"="+sp.Image.Build.Args[k])
	}
	out, err := p.r.pm.Build(ctx, append(args, ctxDir)...)
	if err != nil {
		return "", fmt.Errorf("build: %s", tail(err.Error(), 2000))
	}
	return strings.TrimSpace(string(out)), nil
}

// pinFroms rewrites every FROM that names an image to that image at the
// digest it has on this host (pulling it if the host lacks it).
func (p *placement) pinFroms(ctx context.Context, cf string) (string, error) {
	var out []string
	stages := map[string]bool{}
	for _, line := range strings.Split(cf, "\n") {
		f, ok := spec.ParseFrom(line)
		if !ok {
			out = append(out, line)
			continue
		}
		pinned, err := p.pinFrom(ctx, f, stages)
		if err != nil {
			return "", err
		}
		out = append(out, pinned)
		if f.Name != "" {
			stages[strings.ToLower(f.Name)] = true
		}
	}
	return strings.Join(out, "\n"), nil
}

// pinFrom pins one FROM line; earlier stages, scratch and images already
// pinned are left as they are.
func (p *placement) pinFrom(ctx context.Context, f spec.From, stages map[string]bool) (string, error) {
	if f.Image == "scratch" || stages[strings.ToLower(f.Image)] || strings.Contains(f.Image, "@sha256:") {
		return f.With(f.Image), nil
	}
	if strings.Contains(f.Image, "$") {
		return "", fmt.Errorf("FROM %s: a base image named by a variable cannot be pinned; name it literally", f.Image)
	}
	if err := p.pullIfMissing(ctx, f.Image); err != nil {
		return "", err
	}
	digest, err := p.r.pm.ImageDigest(ctx, f.Image)
	if err != nil {
		return "", err
	}
	return f.With(f.Image + "@" + digest), nil
}

// localBases rewrites a pinned Containerfile's FROMs to a name that is the
// same on every host: localhost/lux-base:<digest>, tagged on the image with
// that digest (under whatever name the host has it, pulled if it has none).
// Builds record how FROM named their base, so naming it identically is what
// makes a rebuild on another host produce the same image.
func (p *placement) localBases(ctx context.Context, cf string) (string, error) {
	byDigest, err := p.r.pm.ImagesByDigest(ctx)
	if err != nil {
		return "", err
	}
	lines := strings.Split(cf, "\n")
	for i, line := range lines {
		f, ok := spec.ParseFrom(line)
		if !ok || !strings.Contains(f.Image, "@sha256:") {
			continue
		}
		_, digest, _ := strings.Cut(f.Image, "@")
		if byDigest[digest] == "" {
			p.event(ctx, "image.pull", map[string]any{"ref": f.Image})
			if err := p.r.pm.Pull(ctx, f.Image); err != nil {
				return "", err
			}
			if byDigest, err = p.r.pm.ImagesByDigest(ctx); err != nil {
				return "", err
			}
			if byDigest[digest] == "" {
				return "", fmt.Errorf("pulled %s but found no image with that digest", f.Image)
			}
		}
		local := "localhost/lux-base:" + strings.TrimPrefix(digest, "sha256:")
		if _, err := p.r.pm.Run(ctx, "tag", byDigest[digest], local); err != nil {
			return "", err
		}
		lines[i] = f.With(local)
	}
	return strings.Join(lines, "\n"), nil
}

// buildKey names a build by what it is built from.
func buildKey(cf string, args map[string]string) string {
	h := sha256.New()
	h.Write([]byte(cf))
	for _, k := range slices.Sorted(maps.Keys(args)) {
		fmt.Fprintf(h, "\x00%s=%s", k, args[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Whole lines from the end.
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "…" + s
}
