package server

import (
	"context"
	"net/http"
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

// notYet answers for a feature that is not built yet, rather than
// accepting work that would never happen.
func (s *Server) notYet(what string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		return errf(http.StatusNotImplemented, "not_implemented", "%s is not implemented yet", what)
	}
}
