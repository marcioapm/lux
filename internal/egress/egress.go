// Package egress enforces a Run's network egress on its host: default deny,
// allowing only the CIDRs and hostnames in its spec.
//
// Each Run gets its own Podman network (a bridge, created with Podman's
// DNS off). One nftables table, `inet lux`, owned by the runner and never
// touched by netavark, holds:
//
//   - a forward chain, at a priority before netavark's, that drops traffic
//     from one Run's bridge to another's (whatever either allows), sends
//     each Run's traffic (by bridge interface) to that Run's chain, and drops
//     traffic from any lux bridge that has none: a Run whose rules are not
//     loaded (a runner restarting) fails closed, not open;
//   - per Run, a chain accepting replies, then dropping what is always
//     blocked (the metadata service, the control plane), then accepting the
//     Run's allow set, then dropping everything else (other Runs' networks
//     and private ranges included, unless the spec allows them);
//   - input rules so a Run reaches the host only on the DNS stub's port;
//   - a DNS redirect: port 53 from any Run goes to the stub.
//
// Hostnames are resolved by the runner (not the container), at start and
// every minute after; addresses are added and never removed while the Run
// lives, so a connection is not cut when a CDN rotates. The DNS stub
// answers only for allowed names (from the same resolution, so an answer
// is always reachable), refuses the rest, and records every lookup.
package egress

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// StubPort is where the DNS stub listens on each Run's gateway.
const StubPort = 15353

// Always blocked, whatever a spec allows: link-local (the cloud metadata
// service) and loopback. Everything else, private ranges included, is
// denied unless the spec allows it.
var alwaysBlocked = []string{"169.254.0.0/16", "127.0.0.0/8"}

const setupRuleset = `
table inet lux {
	map runs {
		type ifname : verdict
	}
	set run_ifaces {
		type ifname
	}
	chain egress {
		type filter hook forward priority -10; policy accept;
		iifname "lux*" oifname "lux*" drop
		iifname vmap @runs
		iifname "lux*" drop
	}
	chain to_host {
		type filter hook input priority -10; policy accept;
		iifname @run_ifaces meta l4proto { tcp, udp } th dport %d accept
		iifname "lux*" ct state established,related accept
		iifname "lux*" drop
	}
	chain dns {
		type nat hook prerouting priority -110; policy accept;
		iifname @run_ifaces meta l4proto { tcp, udp } th dport 53 redirect to :%d
	}
}
`

// Rule is one egress rule from a spec.
type Rule struct {
	Host string
	CIDR string
}

// Lookup is one DNS query a Run made, for its events.
type Lookup struct {
	Name    string   `json:"name"`
	Allowed bool     `json:"allowed"`
	Answers []string `json:"answers,omitempty"`
}

// Firewall is the runner's view of the nftables table and the DNS stubs.
type Firewall struct {
	mu       sync.Mutex
	resolver *net.Resolver
	runs     map[string]*run // by interface
	blocked  []netip.Prefix  // extra always-blocked prefixes (the host, luxd)
}

type run struct {
	id     string
	iface  string
	hosts  map[string]bool // allowed hostnames, lowercased, no trailing dot
	cidrs  []netip.Prefix
	addrs  map[string]map[netip.Addr]bool // resolved, by host; only grows
	stub   *stub
	onDNS  func(Lookup)
	cancel context.CancelFunc
}

// New sets up the nftables table (idempotent: an existing one is replaced,
// so a restarted runner starts clean and re-applies its Runs; meanwhile
// their traffic is dropped, never let through). blocked are
// addresses no Run may reach, besides the always-blocked ranges: the host
// itself, the control plane.
func New(blocked []netip.Prefix) (*Firewall, error) {
	_ = nft("delete table inet lux")
	if err := nftScript(fmt.Sprintf(setupRuleset, StubPort, StubPort)); err != nil {
		return nil, fmt.Errorf("nftables: %w", err)
	}
	return &Firewall{resolver: net.DefaultResolver, runs: map[string]*run{}, blocked: blocked}, nil
}

var nameRe = regexp.MustCompile(`[^a-z0-9_]`)

func chainName(runID string) string {
	return "run_" + nameRe.ReplaceAllString(strings.ToLower(runID), "_")
}
func setName(runID string) string {
	return "allow_" + nameRe.ReplaceAllString(strings.ToLower(runID), "_")
}

// Apply loads a Run's egress rules for its network's bridge interface and
// resolves its allowed hosts. Call it before the container starts: the
// rules match the interface by name, so they hold from the first packet.
// Idempotent. onDNS receives every lookup the Run makes.
func (f *Firewall) Apply(ctx context.Context, runID, iface string, rules []Rule, onDNS func(Lookup)) error {
	f.mu.Lock()
	if old := f.runs[iface]; old != nil {
		f.mu.Unlock()
		f.Remove(iface)
		f.mu.Lock()
	}
	r := &run{id: runID, iface: iface, hosts: map[string]bool{},
		addrs: map[string]map[netip.Addr]bool{}, onDNS: onDNS}
	for _, rule := range rules {
		switch {
		case rule.CIDR != "":
			p, err := netip.ParsePrefix(rule.CIDR)
			if err != nil {
				f.mu.Unlock()
				return fmt.Errorf("egress cidr %q: %w", rule.CIDR, err)
			}
			r.cidrs = append(r.cidrs, p.Masked())
		case rule.Host != "":
			r.hosts[strings.TrimSuffix(strings.ToLower(rule.Host), ".")] = true
		}
	}
	f.runs[iface] = r
	f.mu.Unlock()

	// The Run's chain: replies; then always-blocked; then its own allow
	// set; then drop. Private ranges are blocked unless allowed by CIDR.
	var b strings.Builder
	c, s := chainName(runID), setName(runID)
	fmt.Fprintf(&b, "add set inet lux %s { type ipv4_addr; flags interval; }\n", s)
	fmt.Fprintf(&b, "add chain inet lux %s\n", c)
	fmt.Fprintf(&b, "flush chain inet lux %s\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ct state established,related accept\n", c)
	for _, p := range f.hardBlocked() {
		fmt.Fprintf(&b, "add rule inet lux %s ip daddr %s drop\n", c, p)
	}
	fmt.Fprintf(&b, "add rule inet lux %s ip daddr @%s accept\n", c, s)
	fmt.Fprintf(&b, "add rule inet lux %s counter drop\n", c)
	if len(r.cidrs) > 0 {
		fmt.Fprintf(&b, "add element inet lux %s { %s }\n", s, join(r.cidrs))
	}
	fmt.Fprintf(&b, "add element inet lux runs { %q : jump %s }\n", iface, c)
	fmt.Fprintf(&b, "add element inet lux run_ifaces { %q }\n", iface)
	if err := nftScript(b.String()); err != nil {
		return fmt.Errorf("nftables for %s: %w", runID, err)
	}

	rctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	f.resolveAll(ctx, r)
	go f.reresolve(rctx, r)
	return nil
}

// Open lets a Run's bridge out unfiltered (network.unrestricted): an
// explicit accept, since a lux bridge with no entry is dropped. Other Runs'
// bridges, the metadata service and the control plane stay unreachable.
func (f *Firewall) Open(runID, iface string) error {
	c := chainName(runID)
	var b strings.Builder
	fmt.Fprintf(&b, "add chain inet lux %s\n", c)
	fmt.Fprintf(&b, "flush chain inet lux %s\n", c)
	for _, p := range f.hardBlocked() {
		fmt.Fprintf(&b, "add rule inet lux %s ip daddr %s drop\n", c, p)
	}
	fmt.Fprintf(&b, "add rule inet lux %s accept\n", c)
	fmt.Fprintf(&b, "add element inet lux runs { %q : jump %s }\n", iface, c)
	f.mu.Lock()
	f.runs[iface] = &run{id: runID, iface: iface}
	f.mu.Unlock()
	return nftScript(b.String())
}

// Serve starts a Run's DNS stub on its gateway. Call it once the container
// is up: Podman creates the bridge, and its gateway address, only then.
func (f *Firewall) Serve(iface string, gateway netip.Addr) error {
	f.mu.Lock()
	r := f.runs[iface]
	f.mu.Unlock()
	if r == nil {
		return fmt.Errorf("egress: no rules applied for %s", iface)
	}
	if r.stub != nil {
		r.stub.close()
	}
	st, err := startStub(gateway, r, f)
	if err != nil {
		return fmt.Errorf("dns stub on %s: %w", gateway, err)
	}
	f.mu.Lock()
	r.stub = st
	f.mu.Unlock()
	return nil
}

// hardBlocked lists what no Run may reach, whatever it allows.
func (f *Firewall) hardBlocked() []netip.Prefix {
	out := append([]netip.Prefix{}, f.blocked...)
	for _, p := range alwaysBlocked {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

func (f *Firewall) reresolve(ctx context.Context, r *run) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.resolveAll(ctx, r)
		}
	}
}

// resolveAll resolves every allowed host and adds new addresses to the
// Run's set. Addresses are never removed while it runs.
func (f *Firewall) resolveAll(ctx context.Context, r *run) {
	for h := range r.hosts {
		f.resolve(ctx, r, h)
	}
}

func (f *Firewall) resolve(ctx context.Context, r *run, host string) []netip.Addr {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := f.resolver.LookupNetIP(rctx, "ip4", host)
	if err != nil {
		return f.known(r, host)
	}
	var fresh []netip.Addr
	f.mu.Lock()
	known := r.addrs[host]
	if known == nil {
		known = map[netip.Addr]bool{}
		r.addrs[host] = known
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !known[ip] && !f.isBlocked(r, ip) {
			known[ip] = true
			fresh = append(fresh, ip)
		}
	}
	f.mu.Unlock()
	if len(fresh) > 0 {
		var parts []string
		for _, ip := range fresh {
			parts = append(parts, ip.String())
		}
		_ = nft(fmt.Sprintf("add element inet lux %s { %s }", setName(r.id), strings.Join(parts, ", ")))
	}
	return f.known(r, host)
}

// isBlocked: an allowed hostname that resolves into a blocked range (say
// the metadata address) must not open it.
func (f *Firewall) isBlocked(_ *run, ip netip.Addr) bool {
	for _, p := range f.hardBlocked() {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (f *Firewall) known(r *run, host string) []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []netip.Addr
	for ip := range r.addrs[host] {
		out = append(out, ip)
	}
	return out
}

// Remove tears a Run's egress down (its network is going away).
func (f *Firewall) Remove(iface string) {
	f.mu.Lock()
	r := f.runs[iface]
	delete(f.runs, iface)
	f.mu.Unlock()
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.stub != nil {
		r.stub.close()
	}
	_ = nftScript(fmt.Sprintf("delete element inet lux runs { %q }\ndelete element inet lux run_ifaces { %q }\n", iface, iface))
	_ = nftScript(fmt.Sprintf("delete chain inet lux %s\n", chainName(r.id)))
	_ = nftScript(fmt.Sprintf("delete set inet lux %s\n", setName(r.id)))
}

func join(ps []netip.Prefix) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
}

func nft(cmd string) error { return nftScript(cmd + "\n") }

func nftScript(script string) error {
	c := exec.Command("nft", "-f", "-")
	c.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
