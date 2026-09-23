package server

import (
	"context"
)

// Provider provisions hosts for a pool (EC2).
type Provider interface {
	Name() string
	Launch(ctx context.Context, req LaunchRequest) (providerID string, err error)
	Terminate(ctx context.Context, providerID string) error
}

type LaunchRequest struct {
	Pool     string
	HostName string
	Template map[string]any
	// Token is a host token the new host's runner registers with.
	Token string
}
