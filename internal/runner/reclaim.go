package runner

import (
	"context"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// reclaimGrace is how long a reclaimed spot placement may run after the
// runner first receives the notice: SIGTERM at once, SIGKILL at this.
const reclaimGrace = 15 * time.Second

const stopSpotReclaim = "spot-reclaim"

// reclaimKillAt is the persisted kill instant: stored is the one recorded
// at first receipt (0: none yet), now that receipt.
func reclaimKillAt(stored int64, now time.Time) int64 {
	if stored > 0 {
		return stored
	}
	return now.Add(reclaimGrace).UnixMilli()
}

// reclaim enforces a spot reclamation: the deadline is persisted before
// anything is signalled, the shim is told to SIGTERM now (no beforeStop,
// no grace), and the runner itself kills the container at the deadline,
// whether or not the shim or luxd are reachable.
func (p *placement) reclaim(ctx context.Context) {
	p.mu.Lock()
	var killAt int64
	if p.state != nil {
		p.state.ReclaimKillAt = reclaimKillAt(p.state.ReclaimKillAt, time.Now())
		killAt = p.state.ReclaimKillAt
		_ = p.saveStateLocked()
	} else {
		killAt = reclaimKillAt(0, time.Now())
	}
	armed := p.reclaimArmed
	p.reclaimArmed = true
	p.mu.Unlock()
	if !armed {
		p.armReclaimKill(ctx, time.UnixMilli(killAt))
	}
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimStop, Reason: stopSpotReclaim, KillAtUnixMs: killAt}); err != nil {
		if ctr := p.container(); ctr != "" {
			_ = p.r.pm.Kill(context.WithoutCancel(ctx), ctr, "TERM")
		}
	}
}

// armReclaimKill kills the container (every process in it) at killAt.
func (p *placement) armReclaimKill(ctx context.Context, killAt time.Time) {
	time.AfterFunc(max(0, time.Until(killAt)), func() {
		if ctr := p.container(); ctr != "" {
			_ = p.r.pm.Kill(context.WithoutCancel(ctx), ctr, "KILL")
		}
	})
}
