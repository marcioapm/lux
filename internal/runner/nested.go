package runner

import (
	"context"
	"encoding/json"
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
//   - CAP_SYS_CHROOT (Podman's storage setup chroots), and CAP_SETUID and
//     CAP_SETGID kept as ambient capabilities when the shim switches to the
//     workload user (newuidmap needs them; no-new-privileges stops it from
//     gaining them from its file capabilities);
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
func writeNestedSeccomp(ctx context.Context, dataDir, hostProfile string) (string, error) {
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
func (r *Runner) hostSeccompProfile(ctx context.Context) string {
	out, err := r.pm.Run(ctx, "info", "--format", "{{.Host.Security.SECCOMPProfilePath}}")
	if p := strings.TrimSpace(string(out)); err == nil && p != "" {
		return p
	}
	return "/usr/share/containers/seccomp.json"
}
