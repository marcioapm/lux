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

func (s *Server) serveExec(w http.ResponseWriter, r *http.Request) error {
	return errf(http.StatusNotImplemented, "not_implemented", "exec is not implemented yet")
}

func (s *Server) serveAttach(w http.ResponseWriter, r *http.Request) error {
	return errf(http.StatusNotImplemented, "not_implemented", "attach is not implemented yet")
}

func (s *Server) servePort(w http.ResponseWriter, r *http.Request) error {
	return errf(http.StatusNotImplemented, "not_implemented", "port forwarding is not implemented yet")
}
