package runner

import (
	"context"
	"net/netip"
	"net/url"
	"strings"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

// bridgeName is a Run's bridge interface: fixed, so its egress rules (which
// match by interface) are in place before Podman creates the bridge, and
// "lux"-prefixed, so a bridge with no rules loaded is dropped (see
// package egress). Linux interface names are at most 15 bytes.
func bridgeName(runID string) string {
	id := strings.TrimPrefix(runID, "run_")
	if len(id) > 11 {
		id = id[:11]
	}
	return "lux" + id
}

// setupNetwork creates the Run's network and, unless the spec allows
// unrestricted egress, loads its egress rules before the container exists.
func (p *placement) setupNetwork(ctx context.Context, sp spec.RunSpec) (podman.Network, error) {
	net, err := p.r.pm.NetworkCreate(ctx, networkName(p.runID), bridgeName(p.runID),
		map[string]string{LabelManaged: "true", LabelRun: p.runID})
	if err != nil {
		return net, err
	}
	es := &egressState{Interface: net.Interface, Gateway: net.Gateway.String(), Unrestricted: sp.Network.Unrestricted}
	for _, r := range sp.Network.Egress {
		es.Rules = append(es.Rules, egressRule{Host: r.Host, CIDR: r.CIDR})
	}
	p.mu.Lock()
	p.state.Egress = es
	p.mu.Unlock()
	if err := writeRunState(p.dir, p.state); err != nil {
		return net, err
	}
	return net, p.applyEgress(ctx, es)
}

func (p *placement) applyEgress(ctx context.Context, es *egressState) error {
	if es.Unrestricted {
		return p.r.egress.Open(p.runID, es.Interface)
	}
	rules := make([]egress.Rule, 0, len(es.Rules))
	for _, r := range es.Rules {
		rules = append(rules, egress.Rule{Host: r.Host, CIDR: r.CIDR})
	}
	return p.r.egress.Apply(ctx, p.runID, es.Interface, rules, func(l egress.Lookup) {
		p.event(ctx, "dns", map[string]any{"name": l.Name, "allowed": l.Allowed, "answers": l.Answers})
	})
}

// reapplyEgress restores a re-adopted Run's egress rules and DNS stub.
func (p *placement) reapplyEgress(ctx context.Context) error {
	es := p.state.Egress
	if es == nil {
		return nil
	}
	if err := p.applyEgress(ctx, es); err != nil {
		return err
	}
	if es.Unrestricted {
		return nil
	}
	gw, err := netip.ParseAddr(es.Gateway)
	if err != nil {
		return err
	}
	return p.r.egress.Serve(es.Interface, gw)
}

// serveDNS starts the Run's DNS stub on its gateway, once the container is
// up (Podman creates the bridge, and its address, then).
func (p *placement) serveDNS(sp spec.RunSpec, net podman.Network) error {
	if sp.Network.Unrestricted {
		return nil
	}
	return p.r.egress.Serve(net.Interface, net.Gateway)
}

// blockedForRuns is what no Run may reach on this host, besides the
// always-blocked ranges: the control plane.
func blockedForRuns(luxURL string) []netip.Prefix {
	var out []netip.Prefix
	if u, err := url.Parse(luxURL); err == nil {
		if a, err := netip.ParseAddr(u.Hostname()); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

func dnsServer(sp spec.RunSpec, net podman.Network) string {
	if sp.Network.Unrestricted {
		return "none"
	}
	return net.Gateway.String()
}
