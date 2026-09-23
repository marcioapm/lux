package runner

import (
	"context"

	"github.com/marcioapm/lux/internal/proto"
)

// Features that arrive in later build steps.

func (p *placement) collectArtifacts(ctx context.Context) ([]proto.Artifact, error) { return nil, nil }

func (r *Runner) handleStream(ctx context.Context, f proto.Frame) {}
