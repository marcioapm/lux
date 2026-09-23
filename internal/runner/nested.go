package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/spec"
)

// Nested containers: a Run with sandbox.nestedContainers can run rootless
// Podman inside its container. It gets, beyond every other Run's
// containment, only what that needs, and never --privileged:
//
//   - CAP_SYS_CHROOT (Podman's storage setup chroots);
//   - no no-new-privileges: newuidmap and newgidmap gain CAP_SETUID and
//     CAP_SETGID from their file capabilities, only for themselves. The
//     workload itself never holds them (so it cannot become root in its
//     container), and nothing can gain a capability outside the bounding
//     set every Run has;
//   - /dev/fuse (fuse-overlayfs) and /dev/net/tun (pasta, its network);
//   - unmask=ALL and label=disable (its /proc and /sys mounts);
//   - the host's seccomp profile, plus sethostname, setdomainname and setns:
//     the default profile allows those only with CAP_SYS_ADMIN, which
//     nested containers do not get; inside the Run's user namespace they
//     affect only namespaces the Run itself created.
//
// The container is still in its own user namespace (a unique unprivileged
// host uid range), with its own network and egress rules: nested
// containers share the Run's network namespace's routes, so they have the
// Run's egress and nothing more.
func (p *placement) extraArgs(sp spec.RunSpec) []string {
	if !sp.Sandbox.NestedContainers {
		return nil
	}
	return []string{
		"--cap-add=SYS_CHROOT",
		"--device=/dev/fuse", "--device=/dev/net/tun",
		"--security-opt=unmask=ALL", "--security-opt=label=disable",
		"--security-opt=seccomp=" + p.r.nestedSeccomp,
	}
}

// nestedSyscalls are what the nested profile allows beyond the host's.
var nestedSyscalls = []string{"sethostname", "setdomainname", "setns"}

// writeNestedSeccomp derives the nested-containers seccomp profile from the
// host's default one and writes it under the data directory.
func writeNestedSeccomp(dataDir, hostProfile string) (string, error) {
	b, err := os.ReadFile(hostProfile)
	if err != nil {
		return "", fmt.Errorf("seccomp profile: %w", err)
	}
	var prof map[string]any
	if err := json.Unmarshal(b, &prof); err != nil {
		return "", fmt.Errorf("seccomp profile %s: %w", hostProfile, err)
	}
	rules, _ := prof["syscalls"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if rule["action"] != "SCMP_ACT_ERRNO" {
			continue
		}
		names, _ := rule["names"].([]any)
		kept := names[:0]
		for _, n := range names {
			if s, _ := n.(string); !slices.Contains(nestedSyscalls, s) {
				kept = append(kept, n)
			}
		}
		rule["names"] = kept
	}
	prof["syscalls"] = append(rules, map[string]any{
		"names": nestedSyscalls, "action": "SCMP_ACT_ALLOW", "args": []any{},
		"comment": "lux: nested containers",
	})
	out, err := json.Marshal(prof)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, "nested-seccomp.json")
	return path, writeFileAtomic(path, out, 0o644)
}

// hostSeccompProfile is the profile Podman uses by default on this host.
func (r *Runner) hostSeccompProfile(ctx context.Context) (string, error) {
	out, err := r.pm.Run(ctx, "info", "--format", "{{.Host.Security.SECCOMPProfilePath}}")
	if err != nil {
		return "", fmt.Errorf("podman's seccomp profile: %w", err)
	}
	if p := strings.TrimSpace(string(out)); p != "" {
		return p, nil
	}
	return "", errors.New("podman reports no default seccomp profile: nested containers need one to extend")
}
