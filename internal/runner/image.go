package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

// imageResolved is what the first build of a Run's image recorded, so a
// rebuild (on another host, or after the image was removed) builds from the
// same base images, and a different result is noticed.
type imageResolved struct {
	// Containerfile with every FROM pinned to a digest.
	Containerfile string `json:"containerfile"`
	ImageID       string `json:"imageId"`
}

func (p *placement) ensureImage(ctx context.Context, sp spec.RunSpec, net podman.Network) (string, error) {
	if sp.Image.Build != nil {
		return p.buildImage(ctx, sp, net)
	}
	ref := sp.Image.Ref
	if !p.r.pm.ImageExists(ctx, ref) {
		p.event(ctx, "image.pull", map[string]any{"ref": ref})
		if err := p.r.pm.Pull(ctx, ref); err != nil {
			return "", err
		}
	}
	return ref, nil
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
	var prev imageResolved
	if len(p.assign.ImageResolved) > 0 {
		if err := json.Unmarshal(p.assign.ImageResolved, &prev); err != nil {
			return "", fmt.Errorf("image resolution: %w", err)
		}
	}
	// cf is the pinned Containerfile (recorded, and the cache key); local is
	// what this host builds: the same, with bases under host-neutral names.
	cf := prev.Containerfile
	var err error
	if cf == "" {
		if cf, err = p.pinFroms(ctx, b.Containerfile); err != nil {
			return "", err
		}
	}
	local, err := p.localBases(ctx, cf)
	if err != nil {
		return "", err
	}

	tag := "localhost/lux-build:" + buildKey(cf, b.Args)
	cached := p.r.pm.ImageExists(ctx, tag)
	if !cached {
		p.event(ctx, "image.build", map[string]any{"tag": tag})
		if err := p.build(ctx, sp, local, tag, net); err != nil {
			return "", err
		}
	}
	id, err := p.r.pm.ImageID(ctx, tag)
	if err != nil {
		return "", err
	}
	ev := map[string]any{"tag": tag, "imageId": id, "cached": cached}
	if prev.Containerfile == "" {
		ev["resolved"] = imageResolved{Containerfile: cf, ImageID: id}
	}
	p.event(ctx, "image.built", ev)
	if prev.ImageID != "" && prev.ImageID != id {
		// Not a failure: state volumes do not depend on the image, only the
		// writable layer would, and that does not travel.
		p.event(ctx, "image.rebuild-differs", map[string]any{"imageId": id, "firstImageId": prev.ImageID,
			"hint": "the build is not reproducible (RUN steps that fetch or date things); the Run continues on this image"})
	}
	return tag, nil
}

func (p *placement) build(ctx context.Context, sp spec.RunSpec, cf, tag string, net podman.Network) error {
	tmp := filepath.Join(p.r.cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(tmp, "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// No context yet: the build sees only its Containerfile.
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(cf), 0o600); err != nil {
		return err
	}
	ctxDir := filepath.Join(dir, "context")
	if err := os.Mkdir(ctxDir, 0o755); err != nil {
		return err
	}
	args := []string{"--quiet", "--pull=never", "--isolation=oci", "--layers",
		"--timestamp=0", // no build-time dates in the image: rebuilds match when the steps do
		"--userns=auto:size=65536",
		"--cap-drop=ALL", "--cap-add=CHOWN,DAC_OVERRIDE,FOWNER,SETUID,SETGID,KILL",
		"--security-opt=no-new-privileges",
		"--memory", fmt.Sprintf("%d", int64(sp.Resources.Memory)),
		"--cpu-period=100000", fmt.Sprintf("--cpu-quota=%d", int64(sp.Resources.CPUs*100000)),
		"--network", networkName(p.runID),
		"--label", LabelManaged + "=true",
		"-t", tag, "-f", file,
	}
	args = append(args, dnsArgs(sp, net)...)
	keys := make([]string, 0, len(sp.Image.Build.Args))
	for k := range sp.Image.Build.Args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+sp.Image.Build.Args[k])
	}
	if _, err := p.r.pm.Build(ctx, append(args, ctxDir)...); err != nil {
		return fmt.Errorf("build: %s", tail(err.Error(), 2000))
	}
	return nil
}

// pinFroms rewrites every FROM that names an image to that image at the
// digest it has on this host (pulling it if the host lacks it).
func (p *placement) pinFroms(ctx context.Context, cf string) (string, error) {
	var out []string
	stages := map[string]bool{}
	for _, line := range strings.Split(cf, "\n") {
		f, ok := parseFrom(line)
		if !ok {
			out = append(out, line)
			continue
		}
		pinned, err := p.pinFrom(ctx, f, stages)
		if err != nil {
			return "", err
		}
		out = append(out, pinned)
		if f.name != "" {
			stages[strings.ToLower(f.name)] = true
		}
	}
	return strings.Join(out, "\n"), nil
}

// pinFrom pins one FROM line; earlier stages, scratch and images already
// pinned are left as they are.
func (p *placement) pinFrom(ctx context.Context, f fromLine, stages map[string]bool) (string, error) {
	if f.image == "scratch" || stages[strings.ToLower(f.image)] || strings.Contains(f.image, "@sha256:") {
		return f.with(f.image), nil
	}
	if strings.Contains(f.image, "$") {
		return "", fmt.Errorf("FROM %s: a base image named by a variable cannot be pinned; name it literally", f.image)
	}
	if !p.r.pm.ImageExists(ctx, f.image) {
		p.event(ctx, "image.pull", map[string]any{"ref": f.image})
		if err := p.r.pm.Pull(ctx, f.image); err != nil {
			return "", err
		}
	}
	digest, err := p.r.pm.ImageDigest(ctx, f.image)
	if err != nil {
		return "", err
	}
	return f.with(f.image + "@" + digest), nil
}

// localBases rewrites a pinned Containerfile's FROMs to a name that is the
// same on every host: localhost/lux-base:<digest>, tagged on the image with
// that digest (under whatever name the host has it, pulled if it has none).
// Builds record how FROM named their base, so naming it identically is what
// makes a rebuild on another host produce the same image.
func (p *placement) localBases(ctx context.Context, cf string) (string, error) {
	lines := strings.Split(cf, "\n")
	for i, line := range lines {
		f, ok := parseFrom(line)
		if !ok || !strings.Contains(f.image, "@sha256:") {
			continue
		}
		_, digest, _ := strings.Cut(f.image, "@")
		id := p.r.pm.ImageByDigest(ctx, digest)
		if id == "" {
			p.event(ctx, "image.pull", map[string]any{"ref": f.image})
			if err := p.r.pm.Pull(ctx, f.image); err != nil {
				return "", err
			}
			if id = p.r.pm.ImageByDigest(ctx, digest); id == "" {
				return "", fmt.Errorf("pulled %s but found no image with that digest", f.image)
			}
		}
		local := "localhost/lux-base:" + strings.TrimPrefix(digest, "sha256:")
		if _, err := p.r.pm.Run(ctx, "tag", id, local); err != nil {
			return "", err
		}
		lines[i] = f.with(local)
	}
	return strings.Join(lines, "\n"), nil
}

type fromLine struct {
	flags       []string
	image, name string
}

// parseFrom reads `FROM [--flag=v ...] image [AS name]`.
func parseFrom(line string) (fromLine, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
		return fromLine{}, false
	}
	var f fromLine
	rest := fields[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
		f.flags = append(f.flags, rest[0])
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fromLine{}, false
	}
	f.image = rest[0]
	if len(rest) == 3 && strings.EqualFold(rest[1], "AS") {
		f.name = rest[2]
	}
	return f, true
}

func (f fromLine) with(image string) string {
	parts := append([]string{"FROM"}, f.flags...)
	parts = append(parts, image)
	if f.name != "" {
		parts = append(parts, "AS", f.name)
	}
	return strings.Join(parts, " ")
}

// buildKey names a build by what it is built from.
func buildKey(cf string, args map[string]string) string {
	h := sha256.New()
	h.Write([]byte(cf))
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
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
