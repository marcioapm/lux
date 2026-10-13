package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// reclaimWaitReason is a normal Run's state_reason while spot placements
// it reclaimed release their resources.
const reclaimWaitReason = "waiting for reclaimed spot capacity to be released"

// reclaimable reports whether a normal Run waiting for wait may reclaim
// spot placements: a capacity wait, not a snapshot or chosen-host one.
func reclaimable(wait string) bool {
	return strings.HasPrefix(wait, "waiting for capacity") || wait == "no host matches"
}

func hasPledge(hosts []*candidateHost, runID string) bool {
	return slices.ContainsFunc(hosts, func(h *candidateHost) bool { _, ok := h.Pledges[runID]; return ok })
}

// releasePledge ends runID's pledge now that it is placed (on h, or
// elsewhere: the victims stay signalled, a reclamation is not undone).
func (s *Server) releasePledge(ctx context.Context, tx pgx.Tx, runID string, h *candidateHost) error {
	h.dropPledge(runID)
	_, err := tx.Exec(ctx, `DELETE FROM spot_pledges WHERE run_id = $1`, runID)
	return err
}

// spotWaitReason is a waiting spot Run's state_reason, from its admission
// blockers: tenant-readable, never naming hosts.
func spotWaitReason(blockers []AdmissionBlocker) string {
	var parts []string
	for _, b := range blockers {
		switch b.Code {
		case blockNoHosts:
			parts = append(parts, "no live host in its pool")
		case blockConstraint:
			parts = append(parts, fmt.Sprintf("%d %s its constraints", b.Hosts, plural(b.Hosts, "host fails", "hosts fail")))
		case blockCapability:
			parts = append(parts, fmt.Sprintf("%d %s spot support", b.Hosts, plural(b.Hosts, "host lacks", "hosts lack")))
		case blockNotEnough:
			parts = append(parts, fmt.Sprintf("%d %s spare capacity", b.Hosts, plural(b.Hosts, "host lacks", "hosts lack")))
		case blockNotSettled:
			parts = append(parts, fmt.Sprintf("%d %s spare capacity free for under 60s", b.Hosts, plural(b.Hosts, "host has", "hosts have")))
		case blockNormalPending:
			parts = append(parts, "normal Runs are waiting for that capacity")
		}
	}
	return truncate("waiting for spot capacity: "+strings.Join(parts, ", "), maxWaitReason)
}

// spotPlacement is a live placement of a spot Run, a reclamation candidate.
type spotPlacement struct {
	ID, RunID, TenantID string
	// ReclaimFor: the normal Run it is being reclaimed for, "" if none.
	ReclaimFor string
	Res        spec.Resources
}

// Admission blocker codes, in the order a result lists them.
const (
	blockNoHosts       = "no_hosts"
	blockConstraint    = "constraint"
	blockCapability    = "capability"
	blockNotEnough     = "not_enough_spare"
	blockNotSettled    = "space_not_settled"
	blockNormalPending = "normal_demand_pending"
)

// AdmissionBlocker is why a Run would not start on some hosts now.
type AdmissionBlocker struct {
	Code             string `json:"code" enum:"no_hosts,constraint,capability,not_enough_spare,space_not_settled,normal_demand_pending" doc:"no_hosts: no live connected host in its pool or chosen host; constraint: labels, nesting, tenancy, or another chosen host; capability: the host's lux-runner lacks a feature the Run needs; not_enough_spare: not enough unreserved cpus, memory, disk or run slots; space_not_settled (spot): the spare space has not been continuously free for more than 60s; normal_demand_pending (spot): a queued normal Run could use that space."`
	Hosts            int    `json:"hosts" doc:"How many hosts this blocks."`
	Reason           string `json:"reason,omitempty" doc:"The first such host's reason, for constraint and capability."`
	RemainingSeconds *int   `json:"remainingSeconds,omitempty" doc:"space_not_settled: the shortest wait after which the space would qualify if nothing changes."`
}

// admission is where a Run would start now, or why on no host.
type admission struct {
	Host     *candidateHost
	Blockers []AdmissionBlocker
}

// blockerSet aggregates per-host blockers by code.
type blockerSet map[string]*AdmissionBlocker

func (b blockerSet) add(code, reason string, remaining *time.Duration) {
	e := b[code]
	if e == nil {
		e = &AdmissionBlocker{Code: code, Reason: reason}
		b[code] = e
	}
	e.Hosts++
	if remaining != nil {
		sec := int((*remaining + time.Second - 1) / time.Second)
		if e.RemainingSeconds == nil || sec < *e.RemainingSeconds {
			e.RemainingSeconds = &sec
		}
	}
}

func (b blockerSet) list() []AdmissionBlocker {
	var out []AdmissionBlocker
	for _, code := range []string{blockNoHosts, blockConstraint, blockCapability, blockNotEnough, blockNotSettled, blockNormalPending} {
		if e := b[code]; e != nil {
			out = append(out, *e)
		}
	}
	return out
}

// classify adds h's hostFit blockers to set; ok when there are none.
func (b blockerSet) classify(blockers []fitBlocker) bool {
	if len(blockers) == 0 {
		return true
	}
	for _, f := range blockers {
		switch f.Kind {
		case kindSpotReclaim, kindSyncModes:
			b.add(blockCapability, f.Reason, nil)
			return false
		case kindResource:
		default:
			b.add(blockConstraint, f.Reason, nil)
			return false
		}
	}
	b.add(blockNotEnough, "", nil)
	return false
}

// spotAdmission decides where spot Run r may start now. It is the single
// admission rule for the scheduler and for submit's validateOnly: a host
// that hostFit accepts (constraints, runner capability, current spare),
// whose spare vector covering r has been continuously free for more than
// spotSettle, and that no queued normal Run in demand could use. hosts is
// not modified.
func spotAdmission(r pendingRun, hosts []*candidateHost, demand []pendingRun, now time.Time) admission {
	set := blockerSet{}
	ordered := slices.Clone(hosts)
	slices.SortFunc(ordered, func(a, b *candidateHost) int { return strings.Compare(a.ID, b.ID) })
	res := r.Spec.Resources
	for _, h := range ordered {
		if !set.classify(hostFit(r, h)) {
			continue
		}
		if h.age == nil || !h.age.aged(now).covers(res.CPUs, int64(res.Memory), int64(res.Disk)) {
			var wait *time.Duration
			if h.age != nil {
				if d, ok := h.age.settlesIn(now, res.CPUs, int64(res.Memory), int64(res.Disk)); ok {
					wait = &d
				}
			}
			set.add(blockNotSettled, "", wait)
			continue
		}
		if slices.ContainsFunc(demand, func(n pendingRun) bool { return normalWants(n, h) }) {
			set.add(blockNormalPending, "", nil)
			continue
		}
		return admission{Host: h}
	}
	if len(ordered) == 0 {
		set.add(blockNoHosts, "", nil)
	}
	return admission{Blockers: set.list()}
}

// normalAdmission is where normal Run r fits now (hostFit only), the
// validateOnly answer for a normal Run: it does not account for snapshot
// locality, reclaimable spot placements or hosts a pool could launch.
func normalAdmission(r pendingRun, hosts []*candidateHost) admission {
	set := blockerSet{}
	ordered := slices.Clone(hosts)
	slices.SortFunc(ordered, func(a, b *candidateHost) int { return strings.Compare(a.ID, b.ID) })
	for _, h := range ordered {
		if set.classify(hostFit(r, h.forRun(r.ID))) {
			return admission{Host: h}
		}
	}
	if len(ordered) == 0 {
		set.add(blockNoHosts, "", nil)
	}
	return admission{Blockers: set.list()}
}

// normalWants reports whether queued normal Run n could be placed on h,
// directly or by reclaiming h's spot placements. A Run holding a pledge is
// already served by it.
func normalWants(n pendingRun, h *candidateHost) bool {
	hc := h.clone()
	for _, sp := range h.Spots {
		hc.remove(sp)
	}
	return len(hostFit(n, hc)) == 0
}

func (h *candidateHost) clone() *candidateHost {
	c := *h
	c.Tenants = slices.Clone(h.Tenants)
	return &c
}

// remove simulates sp's resources and tenancy released from h.
func (h *candidateHost) remove(sp spotPlacement) {
	h.UsedCPUs -= sp.Res.CPUs
	h.UsedMem -= int64(sp.Res.Memory)
	h.UsedDisk -= int64(sp.Res.Disk)
	h.UsedRuns--
	if i := slices.Index(h.Tenants, sp.TenantID); i >= 0 {
		h.Tenants = slices.Delete(h.Tenants, i, i+1)
	}
}

// forRun is h as runID sees it: its own pledge's reservation not counted.
func (h *candidateHost) forRun(runID string) *candidateHost {
	extra, ok := h.Pledges[runID]
	if !ok {
		return h
	}
	c := h.clone()
	c.UsedCPUs -= extra.CPUs
	c.UsedMem -= extra.Memory
	c.UsedDisk -= extra.Disk
	c.UsedRuns -= extra.Runs
	return c
}

// pledgeExtra is what a pledge of res reserves beyond its victims' live
// usage on h: per component, what the victims do not hold themselves.
func pledgeExtra(h *candidateHost, runID string, res spec.Resources) freeVec {
	var live freeVec
	for _, sp := range h.Spots {
		if sp.ReclaimFor == runID {
			live.CPUs += sp.Res.CPUs
			live.Memory += int64(sp.Res.Memory)
			live.Disk += int64(sp.Res.Disk)
			live.Runs++
		}
	}
	return freeVec{max(0, res.CPUs-live.CPUs), max(0, int64(res.Memory)-live.Memory), max(0, int64(res.Disk)-live.Disk), max(0, 1-live.Runs)}
}

func (h *candidateHost) addPledge(runID string, extra freeVec) {
	if h.Pledges == nil {
		h.Pledges = map[string]freeVec{}
	}
	h.Pledges[runID] = extra
	h.UsedCPUs += extra.CPUs
	h.UsedMem += extra.Memory
	h.UsedDisk += extra.Disk
	h.UsedRuns += extra.Runs
}

func (h *candidateHost) dropPledge(runID string) {
	extra, ok := h.Pledges[runID]
	if !ok {
		return
	}
	delete(h.Pledges, runID)
	h.UsedCPUs -= extra.CPUs
	h.UsedMem -= extra.Memory
	h.UsedDisk -= extra.Disk
	h.UsedRuns -= extra.Runs
}

// loadPledges counts each pledge's extra reservation (pledgeExtra) as used
// on its host. Pledges are written and deleted only under their host's lock.
func loadPledges(ctx context.Context, tx pgx.Tx, hostIDs []string, byID map[string]*candidateHost) error {
	rows, err := tx.Query(ctx, `SELECT run_id, host_id, tenant_id, resources FROM spot_pledges WHERE host_id = ANY($1) ORDER BY run_id`, hostIDs)
	if err != nil {
		return err
	}
	type pledge struct {
		run, host, tenant string
		res               spec.Resources
	}
	var all []pledge
	for rows.Next() {
		var p pledge
		if err := rows.Scan(&p.run, &p.host, &p.tenant, &p.res); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range all {
		if h := byID[p.host]; h != nil {
			h.addPledge(p.run, pledgeExtra(h, p.run, p.res))
			h.Tenants = append(h.Tenants, p.tenant)
		}
	}
	return nil
}

// ageKey identifies what a host's capacity history holds for: a runner
// hello, a capacity change or a pool move each start it over.
func ageKey(h *candidateHost) string {
	c := h.Capacity
	return fmt.Sprintf("%d/%g/%d/%d/%d/%s", h.CapEpoch, c.CPUs, c.Memory, c.Disk, c.Runs, h.PoolID)
}

// loadCapacityAge reads each host's history (one primary-key read per
// host) and records its current free vector at now.
func (s *Server) loadCapacityAge(ctx context.Context, tx pgx.Tx, hostIDs []string, byID map[string]*candidateHost) error {
	rows, err := tx.Query(ctx, `SELECT host_id, key, history FROM host_capacity_age WHERE host_id = ANY($1)`, hostIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		c := &capacityAge{}
		if err := rows.Scan(&id, &c.Key, &c.History); err != nil {
			rows.Close()
			return err
		}
		if h := byID[id]; h != nil {
			h.age = c
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.now()
	for _, h := range byID {
		if h.age == nil {
			h.age = &capacityAge{}
		}
		h.age.observe(s.id+"/"+ageKey(h), now, freeOf(h))
	}
	return nil
}

// saveCapacityAge writes the histories this transaction changed. Callers
// hold the hosts' locks.
func saveCapacityAge(ctx context.Context, tx pgx.Tx, hosts []*candidateHost) error {
	for _, h := range hosts {
		if h.age == nil || !h.age.dirty {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_capacity_age (host_id, key, history) VALUES ($1, $2, $3)
			ON CONFLICT (host_id) DO UPDATE SET key = excluded.key, history = excluded.history`, h.ID, h.age.Key, h.age.marshal()); err != nil {
			return err
		}
		h.age.dirty = false
	}
	return nil
}

// normalDemandLimit bounds the queued normal Runs read for spot admission;
// a longer queue refuses every spot Run in the batch.
const normalDemandLimit = 500

// normalDemand is every queued normal Run that could use one of hosts
// (their pools, or chosen among them), not holding a pledge. ok is false
// when the queue exceeded normalDemandLimit.
func normalDemand(ctx context.Context, tx pgx.Tx, hosts []*candidateHost) ([]pendingRun, bool, error) {
	var pools, ids []string
	for _, h := range hosts {
		pools = append(pools, h.PoolID)
		ids = append(ids, h.ID)
	}
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, state, spec, pending_sync, coalesce(place_on, ''), pool_id
		FROM runs r WHERE state IN `+queuedRunStates+` AND NOT terminate_requested
		  AND coalesce(spec->>'spot', '') <> 'true'
		  AND (pool_id = ANY($1) OR place_on = ANY($2))
		  AND NOT EXISTS (SELECT 1 FROM spot_pledges sp WHERE sp.run_id = r.id)
		ORDER BY updated_at, id LIMIT $3`, pools, ids, normalDemandLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []pendingRun
	for rows.Next() {
		var r pendingRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.State, &r.Spec, &r.PendingSync, &r.PlaceOn, &r.PoolID); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if len(out) > normalDemandLimit {
		return nil, false, rows.Err()
	}
	return out, true, rows.Err()
}

// reclaimPlan picks, on one host, a deterministic small set of unclaimed
// spot placements whose release makes normal Run r fit (hostFit on the host
// with them removed, tenancy included). Hosts are tried in id order; on
// each, the youngest placements are removed until r fits, then any victim
// r fits without is put back. nil when no host's spot placements suffice.
func reclaimPlan(r pendingRun, hosts []*candidateHost) (*candidateHost, []spotPlacement) {
	ordered := slices.Clone(hosts)
	slices.SortFunc(ordered, func(a, b *candidateHost) int { return strings.Compare(a.ID, b.ID) })
	for _, h := range ordered {
		var free []spotPlacement
		for _, sp := range h.Spots {
			if sp.ReclaimFor == "" {
				free = append(free, sp)
			}
		}
		if len(free) == 0 {
			continue
		}
		all := h.clone()
		for _, sp := range free {
			all.remove(sp)
		}
		if len(hostFit(r, all)) != 0 {
			continue
		}
		// Spots are loaded oldest first: the youngest lose the least work.
		slices.Reverse(free)
		sim := h.clone()
		var chosen []spotPlacement
		for _, sp := range free {
			sim.remove(sp)
			chosen = append(chosen, sp)
			if len(hostFit(r, sim)) == 0 {
				break
			}
		}
		for i := 0; i < len(chosen); {
			try := h.clone()
			for j, sp := range chosen {
				if j != i {
					try.remove(sp)
				}
			}
			if len(chosen) > 1 && len(hostFit(r, try)) == 0 {
				chosen = slices.Delete(chosen, i, i+1)
				continue
			}
			i++
		}
		return h, chosen
	}
	return nil, nil
}

// pledge claims victims on h for normal Run r and reserves r's resources
// there, in the scheduler's transaction under h's lock. A victim whose row
// another transaction holds is not waited for: the plan is abandoned
// (false) and retried on a later pass.
func pledge(ctx context.Context, tx pgx.Tx, r pendingRun, h *candidateHost, victims []spotPlacement) (bool, error) {
	ids := make([]string, len(victims))
	for i, v := range victims {
		ids[i] = v.ID
	}
	rows, err := tx.Query(ctx, `SELECT id FROM placements WHERE id = ANY($1) AND reclaim_for IS NULL AND state IN `+livePlacementStates+`
		ORDER BY id FOR UPDATE SKIP LOCKED`, ids)
	if err != nil {
		return false, err
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(locked) != len(ids) {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE placements SET reclaim_for = $2 WHERE id = ANY($1)`, ids, r.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO spot_pledges (run_id, host_id, tenant_id, resources) VALUES ($1, $2, $3, $4)`,
		r.ID, h.ID, r.TenantID, r.Spec.Resources); err != nil {
		return false, err
	}
	for i := range h.Spots {
		if slices.Contains(ids, h.Spots[i].ID) {
			h.Spots[i].ReclaimFor = r.ID
		}
	}
	h.addPledge(r.ID, pledgeExtra(h, r.ID, r.Spec.Resources))
	h.Tenants = append(h.Tenants, r.TenantID)
	return true, nil
}

// dropPledges deletes the pledges of Runs no longer waiting for a host, on
// hosts this transaction holds, and of hosts no longer live.
func (s *Server) dropPledges(ctx context.Context, tx pgx.Tx, hostIDs []string) error {
	_, err := tx.Exec(ctx, `DELETE FROM spot_pledges sp WHERE
		(sp.host_id = ANY($1) AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = sp.run_id
			AND r.state IN `+queuedRunStates+` AND NOT r.terminate_requested))
		OR (sp.host_id = ANY($1) AND NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id = sp.host_id AND h.state = 'ready' AND NOT h.draining
			AND h.last_heartbeat > now() - $2::interval))`, hostIDs, interval(s.cfg.LeaseDuration))
	return err
}

// signalReclaims sends the stop for every claimed spot placement not yet
// told: the Run's row first, then its host's (requestStop), never the other
// way round. The claim is durable, so a crash between the scheduler's
// commit and this pass is caught up by the next.
func (s *Server) signalReclaims(ctx context.Context) error {
	type claim struct{ run, tenant, host, forRun string }
	var claims []claim
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT run_id, tenant_id, host_id, reclaim_for FROM placements
			WHERE reclaim_for IS NOT NULL AND stop_reason <> $1 AND state IN `+livePlacementStates+` ORDER BY run_id`, stopSpotReclaim)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c claim
			if err := rows.Scan(&c.run, &c.tenant, &c.host, &c.forRun); err != nil {
				rows.Close()
				return err
			}
			claims = append(claims, c)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, c := range claims {
		var host string
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, c.run); err != nil {
				return err
			}
			var err error
			if host, err = s.requestStop(ctx, tx, c.tenant, c.run, stopSpotReclaim); err != nil || host == "" {
				return err
			}
			if err := addEvent(ctx, tx, c.tenant, c.run, 0, "spot.reclaimed", map[string]any{"for": c.forRun, "host": host}); err != nil {
				return err
			}
			return hostEvent(ctx, tx, host, evSpotReclaimed, map[string]any{"run": c.run, "for": c.forRun})
		})
		if err != nil {
			return err
		}
		if host != "" {
			s.hub.Notify(host)
		}
	}
	return nil
}
