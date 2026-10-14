package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/runner/disklimit"
	"github.com/marcioapm/lux/internal/spec"
)

// FreeMark is a free-space watermark: a percentage of the filesystem's
// capacity (Percent, in (0, 100)) or a number of bytes (Bytes > 0). The
// zero value is unset.
type FreeMark struct {
	Percent float64
	Bytes   int64
}

// Default watermarks of the low-free-space guard. Conservative: no shaped
// host has measured them yet.
var (
	DefaultDiskLowFree     = FreeMark{Percent: 5}
	DefaultDiskRecoverFree = FreeMark{Percent: 10}
)

// ParseFreeMark reads "5%" (a percentage of capacity, above 0 and below
// 100) or a size ("8GiB", "8Gi", a number of bytes, above 0). Its error
// does not repeat s: the caller names the flag or variable and the value.
func ParseFreeMark(s string) (FreeMark, error) {
	s = strings.TrimSpace(s)
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		p, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || math.IsNaN(p) || p <= 0 || p >= 100 {
			return FreeMark{}, errors.New("want a percentage above 0% and below 100%")
		}
		return FreeMark{Percent: p}, nil
	}
	b, err := spec.ParseBytes(s)
	if err != nil || b <= 0 {
		return FreeMark{}, errors.New("want a percentage (5%) or a size above 0 (8GiB)")
	}
	return FreeMark{Bytes: int64(b)}, nil
}

func (m FreeMark) IsZero() bool { return m == FreeMark{} }

func (m FreeMark) String() string {
	if m.Percent > 0 {
		return strconv.FormatFloat(m.Percent, 'f', -1, 64) + "%"
	}
	return spec.BytesText(m.Bytes)
}

// of is the mark in bytes of a filesystem of capacity bytes, rounded down.
func (m FreeMark) of(capacity int64) int64 {
	if m.Percent > 0 {
		return int64(float64(capacity) * m.Percent / 100)
	}
	return m.Bytes
}

// CheckFreeMarks refuses a low mark not below the recovery mark, when the
// two can be compared without the filesystem (same unit). newDiskGuard
// checks the rest against the filesystem's capacity at start.
func CheckFreeMarks(low, rec FreeMark) error {
	switch {
	case low.Percent > 0 && rec.Percent > 0 && low.Percent >= rec.Percent,
		low.Bytes > 0 && rec.Bytes > 0 && low.Bytes >= rec.Bytes:
		return fmt.Errorf("--disk-low-free %s must be below --disk-recover-free %s", low, rec)
	}
	return nil
}

// diskGuard is the low-free-space guard of a runner that scales disk. Each
// pass measures the free bytes of the filesystem holding the data
// directory. Pressure starts when they fall below the low mark and ends
// only once they are at or above the recovery mark. Under pressure a pass:
//
//  1. blocks admissions: the heartbeat carries the pressure, and luxd
//     places nothing more here (published at once when pressure starts);
//  2. reclaims caches: lux's unused images, as --image-disk-high does, up
//     to the recovery mark;
//  3. asks luxd to stop placements over their effective disk limit (within
//     their grace), largest excess first, until the excess of those
//     already stopping covers what the filesystem lacks of the recovery
//     mark, measured again after each one. A placement at or under its
//     limit is never stopped.
//
// A pass that finds nothing to reclaim, no placement stopping and none to
// stop reports the pressure as unrelieved.
type diskGuard struct {
	log                *slog.Logger
	lowMark, recMark   FreeMark
	lowFree, recovFree int64

	// free measures the filesystem's free bytes (unprivileged).
	free func() (int64, error)
	// publish sends a heartbeat now, carrying state().
	publish func(ctx context.Context)
	// reclaim removes unused caches toward freeing need bytes; whether it
	// removed anything.
	reclaim func(ctx context.Context, need int64) bool
	// victims are the placements over their effective disk limit.
	victims func() []diskVictim
	// stop asks luxd to stop v, under pressure with free bytes left.
	stop func(ctx context.Context, v diskVictim, free int64)

	mu       sync.Mutex
	pressure *proto.DiskPressure
}

// diskVictim is a placement over its effective disk limit: used bytes at
// its latest sample against limit. stopping: it is already on its way out
// (stopping, reported over its grace, or asked to stop by this guard).
type diskVictim struct {
	runID    string
	epoch    int
	used     int64
	limit    disklimit.Limit
	stopping bool
	p        *placement
}

func (v diskVictim) excess() int64 { return v.used - v.limit.Effective }

// newDiskGuard resolves low and rec against the filesystem's capacity and
// refuses marks that are not 0 < low < recover < capacity.
func newDiskGuard(log *slog.Logger, low, rec FreeMark, capacity int64) (*diskGuard, error) {
	lo, re := low.of(capacity), rec.of(capacity)
	if lo <= 0 || lo >= re || re >= capacity {
		return nil, fmt.Errorf("--disk-low-free %s (%d bytes) and --disk-recover-free %s (%d bytes) must satisfy 0 < low < recover < the filesystem's capacity, %d bytes",
			low, lo, rec, re, capacity)
	}
	return &diskGuard{log: log, lowMark: low, recMark: rec, lowFree: lo, recovFree: re}, nil
}

// state is the heartbeat's pressure: a copy, nil when not under pressure.
func (g *diskGuard) state() *proto.DiskPressure {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pressure == nil {
		return nil
	}
	c := *g.pressure
	return &c
}

// observe applies one measurement to the hysteresis: entered when free
// fell below the low mark, left when it is back at the recovery mark.
func (g *diskGuard) observe(free int64) (entered, left bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.pressure == nil && free < g.lowFree:
		g.pressure = &proto.DiskPressure{FreeBytes: free, LowFreeBytes: g.lowFree, RecoverFreeBytes: g.recovFree,
			LowFree: g.lowMark.String(), RecoverFree: g.recMark.String()}
		return true, false
	case g.pressure != nil && free >= g.recovFree:
		g.pressure = nil
		return false, true
	case g.pressure != nil:
		g.pressure.FreeBytes = free
	}
	return false, false
}

// setNoRelief records whether the latest pass found any relief, and
// reports whether that changed.
func (g *diskGuard) setNoRelief(free int64, none bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pressure == nil {
		return false
	}
	g.pressure.FreeBytes = free
	changed := g.pressure.NoRelief != none
	g.pressure.NoRelief = none
	return changed
}

// pass measures the filesystem and, under pressure, acts as diskGuard
// says.
func (g *diskGuard) pass(ctx context.Context) {
	free, err := g.free()
	if err != nil {
		g.log.Warn("disk guard: measuring the filesystem failed", "err", err)
		return
	}
	entered, left := g.observe(free)
	switch {
	case left:
		g.log.Info("disk pressure ended", "free", free, "recoverFree", g.recovFree)
		g.publish(ctx)
		return
	case entered:
		g.log.Warn("disk pressure: admissions blocked", "free", free, "lowFree", g.lowFree, "recoverFree", g.recovFree)
		g.publish(ctx)
	case g.state() == nil:
		return
	}
	relieved := g.reclaim(ctx, g.recovFree-free)
	if relieved {
		if free, err = g.free(); err != nil {
			g.log.Warn("disk guard: measuring the filesystem failed", "err", err)
			return
		}
	}
	var pending int64
	var cands []diskVictim
	for _, v := range g.victims() {
		if v.excess() <= 0 {
			continue
		}
		if v.stopping {
			pending += v.excess()
			relieved = true
			continue
		}
		cands = append(cands, v)
	}
	slices.SortFunc(cands, func(a, b diskVictim) int {
		return cmp.Or(cmp.Compare(b.excess(), a.excess()), strings.Compare(a.runID, b.runID))
	})
	for _, v := range cands {
		if g.recovFree-free-pending <= 0 {
			break
		}
		g.log.Warn("disk pressure: stopping a placement over its disk limit", "run", v.runID, "epoch", v.epoch,
			"used", v.used, "limit", v.limit.Effective, "excess", v.excess(), "free", free)
		g.stop(ctx, v, free)
		relieved = true
		pending += v.excess()
		if f, err := g.free(); err == nil {
			free = f
		}
	}
	if g.setNoRelief(free, !relieved) {
		if !relieved {
			g.log.Warn("disk pressure: nothing to reclaim and no placement over its disk limit; admissions stay blocked", "free", free)
		}
		g.publish(ctx)
	}
}

// attachGuard wires g to this runner: free measures the filesystem, and
// the rest acts on the runner's images, placements and heartbeat.
func (r *Runner) attachGuard(g *diskGuard, free func() (int64, error)) {
	g.free = free
	g.publish = r.sendHeartbeat
	g.reclaim = func(ctx context.Context, need int64) bool {
		removed := r.reclaimImages(ctx, need)
		if len(removed) > 0 {
			r.log.Warn("disk pressure: removed unused images", "images", removed)
		}
		return len(removed) > 0
	}
	g.victims = r.diskVictims
	g.stop = r.pressureStop
	r.guard = g
}

// guardPass runs the guard, if on: usageLoop calls it after each round of
// disk samples.
func (r *Runner) guardPass(ctx context.Context) {
	if r.guard != nil {
		r.guard.pass(ctx)
	}
}

// diskVictims are the live placements whose latest conclusive disk sample
// was over their effective limit.
func (r *Runner) diskVictims() []diskVictim {
	var out []diskVictim
	for _, p := range r.livePlacements() {
		l := p.diskLimit()
		if l.Nominal <= 0 {
			continue
		}
		p.mu.Lock()
		v := diskVictim{runID: p.runID, epoch: p.epoch, used: p.lastDisk, limit: l, p: p,
			stopping: p.phase != "running" || p.stopWhy != "" || p.pressureStop || p.disk.Reported()}
		p.mu.Unlock()
		if v.excess() > 0 {
			out = append(out, v)
		}
	}
	return out
}

// pressureStop marks v's placement as stopped by the guard and reports it
// to luxd, which stops it (and retries until luxd has it).
func (r *Runner) pressureStop(ctx context.Context, v diskVictim, free int64) {
	p := v.p
	p.mu.Lock()
	p.pressureStop = true
	p.mu.Unlock()
	g := r.guard
	data := map[string]any{"usedBytes": v.used, "limitBytes": v.limit.Effective, "excessBytes": v.excess(),
		"freeBytes": free, "lowFreeBytes": g.lowFree, "recoverFreeBytes": g.recovFree}
	if v.limit.Scaled {
		data["nominalLimitBytes"], data["limitFactor"] = v.limit.Nominal, v.limit.Factor
	}
	go p.reportRetrying(context.WithoutCancel(ctx), proto.RunEvent{Type: proto.EvDiskPressureStop, Data: data})
}

// sendHeartbeat sends one heartbeat now. Heartbeats are serialized: the
// guard publishes a change of pressure between the loop's beats.
func (r *Runner) sendHeartbeat(ctx context.Context) {
	r.hbMu.Lock()
	defer r.hbMu.Unlock()
	hb := proto.Heartbeat{LocalSnapshots: r.localSnapshots(), GitMirrors: r.git.Mirrors(),
		RunnerSHA256: r.runnerSHA256, ShimSHA256: r.shimSHA256}
	if hu, err := podman.ReadHostUsage(r.cfg.DataDir); err == nil {
		hb.Usage = r.hostUsage(hu)
	}
	if p, err := r.proc.Read(); err == nil {
		hb.Runner = p
	}
	for _, p := range r.livePlacements() {
		if st := p.liveState(); st != "" {
			hb.Leases = append(hb.Leases, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st, Usage: p.usage()})
		}
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Acked: its peak RSS reached luxd. Else it carries to the next.
	if r.conn.Report(hctx, proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(hb)}) == nil {
		r.proc.Stored()
	}
}
