package main

import (
	"context"
	"log/slog"

	"github.com/marcioapm/lux/internal/server"
)

// providers builds the host provisioners luxd can use. Filled in with the
// EC2 provider.
func providers(ctx context.Context, log *slog.Logger) (map[string]server.Provider, error) {
	return map[string]server.Provider{}, nil
}
