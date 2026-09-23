package runner

import (
	"context"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Features that arrive in later build steps.

func (p *placement) extraArgs(sp spec.RunSpec) []string { return nil }

func (p *placement) collectArtifacts(ctx context.Context) ([]proto.Artifact, error) { return nil, nil }

func (r *Runner) handleStream(ctx context.Context, f proto.Frame) {}
