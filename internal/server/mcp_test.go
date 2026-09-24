package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

func TestMCPNotControlPlane(t *testing.T) {
	s := &Server{cfg: Config{PublicURL: "http://10.88.0.1:8443"}}
	for url, refused := range map[string]bool{
		"http://10.88.0.1:9999/mcp":   true, // same address, another port
		"http://10.88.0.1/mcp":        true,
		"http://localhost:8080/mcp":   false,
		"http://10.88.0.2:8080/mcp":   false,
		"http://no-such-host.invalid": false, // a failed lookup is no error
	} {
		sp := spec.RunSpec{Workload: spec.Workload{MCPServers: []spec.MCPServer{{Name: "m", URL: url}}}}
		err := s.checkMCPNotControlPlane(context.Background(), sp)
		var he *HTTPError
		switch {
		case refused && (!errors.As(err, &he) || he.Status != 422 || !strings.Contains(he.Message, "control plane")):
			t.Errorf("%s: want 422, got %v", url, err)
		case !refused && err != nil:
			t.Errorf("%s: %v", url, err)
		}
	}
	// By name: luxd's host, and a name resolving to its address.
	s.cfg.PublicURL = "http://localhost:8443"
	for _, url := range []string{"http://LOCALHOST:1/mcp", "http://127.0.0.1:1/mcp"} {
		sp := spec.RunSpec{Workload: spec.Workload{MCPServers: []spec.MCPServer{{Name: "m", URL: url}}}}
		if err := s.checkMCPNotControlPlane(context.Background(), sp); err == nil {
			t.Errorf("%s: not refused", url)
		}
	}
}
