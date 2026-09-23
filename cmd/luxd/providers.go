package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/marcioapm/lux/internal/ec2"
	"github.com/marcioapm/lux/internal/server"
)

// providers builds the host provisioners luxd can use: EC2, with the
// standard AWS configuration (LUX_EC2_ENDPOINT overrides the endpoint).
func providers(ctx context.Context, log *slog.Logger) (map[string]server.Provider, error) {
	return map[string]server.Provider{"ec2": ec2.New(os.Getenv("LUX_EC2_ENDPOINT"))}, nil
}
