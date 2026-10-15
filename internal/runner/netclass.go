package runner

import (
	"context"
	"maps"
	"net/url"

	"github.com/marcioapm/lux/internal/proto"
)

// netBaseline survives a runner restart along with the kernel counters.
// A decrease folds the last kernel value into Base; max alone would hide
// traffic until the new counter overtook its old high-water mark.
type netBaseline struct {
	Base map[string]proto.NetBytes `json:"base,omitempty"`
	Last map[string]proto.NetBytes `json:"last,omitempty"`
}

func (n *netBaseline) observe(current map[string]proto.NetBytes) map[string]proto.NetBytes {
	if n.Base == nil {
		n.Base = map[string]proto.NetBytes{}
	}
	// Missing zero counters still participate in reset detection.
	keys := maps.Clone(n.Last)
	if keys == nil {
		keys = map[string]proto.NetBytes{}
	}
	for k, v := range current {
		keys[k] = v
	}
	for k := range keys {
		c, last, b := current[k], n.Last[k], n.Base[k]
		if c.Rx < last.Rx {
			b.Rx += last.Rx
		}
		if c.Tx < last.Tx {
			b.Tx += last.Tx
		}
		n.Base[k] = b
	}
	n.Last = maps.Clone(current)
	return n.value()
}

func (n *netBaseline) value() map[string]proto.NetBytes {
	out := maps.Clone(n.Base)
	if out == nil {
		out = map[string]proto.NetBytes{}
	}
	for k, v := range n.Last {
		b := out[k]
		b.Rx += v.Rx
		b.Tx += v.Tx
		out[k] = b
	}
	for k, v := range out {
		if v.Rx == 0 && v.Tx == 0 {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reconcile raises the reported floor from Live without adding the same
// surviving kernel counter twice. Called after a current kernel reading.
func (n *netBaseline) reconcile(reported map[string]proto.NetBytes) {
	if n.Base == nil {
		n.Base = map[string]proto.NetBytes{}
	}
	for k, v := range reported {
		b, c := n.Base[k], n.Last[k]
		b.Rx = max(b.Rx, v.Rx-c.Rx)
		b.Tx = max(b.Tx, v.Tx-c.Tx)
		n.Base[k] = b
	}
}

func (p *placement) observeNet(current map[string]proto.NetBytes) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.netClasses.observe(current)
	if p.state != nil {
		p.state.Net = p.netClasses
		if err := p.saveStateLocked(); err != nil {
			p.logf("save network baseline", "err", err)
		}
	}
}

// sampleNetwork serializes nft snapshots with final reads: an older tick
// cannot arrive after Remove's final reading and look like a reset.
func (r *Runner) sampleNetwork() {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.sampleNetworkLocked()
}

func (r *Runner) sampleNetworkLocked() {
	if r.egress == nil {
		return
	}
	c, err := r.egress.Counters()
	if err != nil {
		r.log.Warn("network counters", "err", err)
		return
	}
	r.hostNet.observe(c.Host)
	for _, p := range r.livePlacements() {
		if current, ok := c.Runs[bridgeName(p.runID)]; ok {
			p.observeNet(current)
		}
	}
}

func (p *placement) removeNetwork() {
	r := p.r
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.egress == nil {
		return
	}
	if final := r.egress.Remove(bridgeName(p.runID)); final != nil {
		p.observeNet(final)
	}
}

// Welcome carries the full catalog; a changed heartbeat version triggers
// one host-token-authenticated GET, never a provider call in hello.
func (r *Runner) refreshNetwork(ctx context.Context, version string) {
	if r.egress == nil || r.api == nil || r.egress.ClassesVersion() == version {
		return
	}
	var nc proto.NetworkClasses
	if err := r.api.getJSON(ctx, "/runner/v1/network?host="+url.QueryEscape(r.cfg.Name), &nc); err != nil {
		r.log.Warn("network catalog", "err", err)
		return
	}
	if err := r.egress.SetClasses(&nc); err != nil {
		r.log.Warn("network catalog", "err", err)
	}
}
