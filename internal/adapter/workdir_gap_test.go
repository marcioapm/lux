package adapter

import (
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// TestWorkdirFallsBackToTheWorkloadUsersHome is a known gap, skipped: with
// workload.workdir unset, workdir() roots an ACP, OpenCode or Codex session
// at the HOME of the process that calls it, lux-shim's (root's, or the
// image's ENV HOME), not the workload user's home where the workload
// itself starts. Closing it is a runtime change; remove the skip with it.
func TestWorkdirFallsBackToTheWorkloadUsersHome(t *testing.T) {
	t.Skip("known gap: workdir() falls back to lux-shim's HOME, not the workload user's home")
	t.Setenv("HOME", "/root") // lux-shim's own, as container root
	cfg := proto.ShimConfig{User: "1000:1000"}
	if got := workdir(cfg); got == "/root" {
		t.Fatalf("workdir() = %q: lux-shim's HOME, not the workload user's home", got)
	}
}
