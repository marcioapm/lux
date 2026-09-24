package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// checkMCPNotControlPlane refuses MCP servers on luxd's own host: every
// runner blocks the control plane (every address its URL's host resolves
// to), so the agent could never reach it. Refused here rather than failing
// in the Run. Lookups are best effort: one that fails proves nothing, and
// the runner's block holds anyway.
func (s *Server) checkMCPNotControlPlane(ctx context.Context, sp spec.RunSpec) error {
	if len(sp.Workload.MCPServers) == 0 || s.cfg.PublicURL == "" {
		return nil
	}
	pu, err := url.Parse(s.cfg.PublicURL)
	if err != nil || pu.Hostname() == "" {
		return nil
	}
	luxd := lookupAddrs(ctx, pu.Hostname())
	for _, m := range sp.Workload.MCPServers {
		u, err := url.Parse(m.URL)
		if err != nil {
			continue
		}
		host := u.Hostname()
		same := strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(pu.Hostname(), "."))
		for a := range lookupAddrs(ctx, host) {
			same = same || luxd[a]
		}
		if same {
			return errf(http.StatusUnprocessableEntity, "invalid_request",
				"workload.mcpServers %q: %s is the control plane's address, which a Run can never reach: run the MCP server elsewhere",
				m.Name, host)
		}
	}
	return nil
}

// lookupAddrs is host's addresses (itself, for an IP literal); none when
// the lookup fails.
func lookupAddrs(ctx context.Context, host string) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	if a, err := netip.ParseAddr(host); err == nil {
		out[a.Unmap()] = true
		return out
	}
	// Each lookup has its own budget: a slow one does not skip the rest.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, _ := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	for _, a := range addrs {
		out[a.Unmap()] = true
	}
	return out
}
