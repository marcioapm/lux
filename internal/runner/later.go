package runner

import (
	"context"
	"errors"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Features that arrive in later build steps.

func (p *placement) buildImage(ctx context.Context, sp spec.RunSpec) (string, error) {
	return "", errors.New("image.build is not supported yet")
}

func (p *placement) extraArgs(sp spec.RunSpec) []string { return nil }

func (p *placement) collectArtifacts(ctx context.Context) ([]proto.Artifact, error) { return nil, nil }

func (r *Runner) handleStream(ctx context.Context, f proto.Frame) {}
