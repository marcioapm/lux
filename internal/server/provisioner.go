package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Provider provisions hosts for pools whose provider it is (ec2).
type Provider interface {
	// Launch starts one host and returns its provider id. template is the
	// pool's; env is what its runner starts with (URL, host token, name).
	Launch(ctx context.Context, pool, hostName string, template json.RawMessage, env map[string]string) (string, error)
	// Terminate ends a host. One that no longer exists is done.
	Terminate(ctx context.Context, template json.RawMessage, providerID string) error
	// Gone reports which of the given hosts the provider knows are
	// terminated. A host it does not know yet (just launched) is not gone.
	Gone(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]bool, error)
}

// provisionerLoop keeps provisioned pools the size their demand, minimum
// and warm settings say. One luxd at a time does it (an advisory lock).
func (s *Server) provisionerLoop(ctx context.Context) {
	if len(s.cfg.Providers) == 0 {
		return
	}
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.provision(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("provisioner", "err", err)
		}
	}
}

type poolRow struct {
	ID, Name, Provider string
	TenantID           *string
	Template           json.RawMessage
	Min, Max, Warm     int
	Retired            bool
}

// provisionLock: the provisioner's advisory lock key ("lux/prov").
const provisionLock = 0x6c75782f70726f76

func (s *Server) provision(ctx context.Context) error {
	// A session lock, held on one connection for the whole pass: another
	// luxd must not reconcile (and launch) at the same time.
	conn, err := s.db.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(provisionLock)).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(provisionLock))

	var pools []poolRow
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, provider, tenant_id, template, min_hosts, max_hosts, warm_hosts, retired
			FROM pools WHERE provider <> 'static'
			  AND (NOT retired OR EXISTS (SELECT 1 FROM hosts h WHERE h.pool = pools.name
			       AND coalesce(h.tenant_id, '') = coalesce(pools.tenant_id, '') AND h.state <> 'terminated'))`)
		if err != nil {
			return err
		}
		pools, err = pgx.CollectRows(rows, pgx.RowToStructByPos[poolRow])
		return err
	})
	if err != nil {
		return err
	}
	checkAlive := time.Since(s.lastAliveCheck) > time.Minute
	if checkAlive {
		s.lastAliveCheck = time.Now()
	}
	for _, pl := range pools {
		prov := s.cfg.Providers[pl.Provider]
		if prov == nil {
			continue
		}
		if err := s.reconcilePool(ctx, prov, pl, checkAlive); err != nil {
			s.log.Warn("pool", "pool", pl.Name, "err", err)
		}
	}
	return nil
}

// poolState is what a pool has and needs, counted in one transaction.
type poolState struct {
	demand, idle, provisioning, total int
	// Idle hosts that have been idle longer than the cooldown, oldest first.
	idleExpired []string
	// Hosts to terminate now: drained ones that are done (no live
	// placements, nothing to upload), ones that never registered, and lost
	// ones (their runner stopped answering; the instance may still run).
	terminate []hostRef
	// Hosts the provider should still have (checked once a minute).
	existing []hostRef
}

// hostRef is a provisioned host, with the template it was launched with
// (its region), whatever its pool's template says now.
type hostRef struct {
	ID, ProviderID, Reason string
	Template               json.RawMessage
}

func (s *Server) reconcilePool(ctx context.Context, prov Provider, pl poolRow, checkAlive bool) error {
	var st poolState
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.poolState(ctx, tx, pl, &st)
	})
	if err != nil {
		return err
	}

	// Hosts the provider says are terminated (behind our back): their rows
	// go, and their Runs are lost by the usual heartbeat path. Checked once
	// a minute (provider API limits).
	if checkAlive && len(st.existing) > 0 {
		for tmpl, hosts := range byTemplate(st.existing) {
			pids := make([]string, len(hosts))
			for i, h := range hosts {
				pids[i] = h.ProviderID
			}
			gone, err := prov.Gone(ctx, json.RawMessage(tmpl), pids)
			if err != nil {
				s.log.Warn("provider check", "pool", pl.Name, "err", err)
				continue
			}
			for _, h := range hosts {
				if gone[h.ProviderID] {
					s.markTerminated(ctx, h.ID, "the provider terminated this host")
					st.total--
				}
			}
		}
	}

	// Scale down: drain idle hosts beyond what warm and waiting Runs need
	// (and never below the minimum), terminate what is done.
	for _, id := range st.idleExpired {
		if st.idle <= pl.Warm+st.demand || st.total <= pl.Min {
			break
		}
		drained, err := s.drainForScaleDown(ctx, id)
		if err != nil {
			return err
		}
		if drained {
			st.total--
			st.idle--
		}
	}
	for _, h := range st.terminate {
		if err := prov.Terminate(ctx, h.Template, h.ProviderID); err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			continue
		}
		s.markTerminated(ctx, h.ID, h.Reason)
	}

	// Scale up: enough for what waits plus the warm hosts, at least the
	// minimum, never past the maximum. A retired pool only winds down.
	if pl.Retired {
		return nil
	}
	want := max(pl.Min-st.total, pl.Warm+st.demand-st.idle-st.provisioning)
	if pl.Max > 0 {
		want = min(want, pl.Max-st.total)
	}
	for range max(want, 0) {
		if err := s.launch(ctx, prov, pl); err != nil {
			return err
		}
	}
	return nil
}

func byTemplate(hosts []hostRef) map[string][]hostRef {
	out := map[string][]hostRef{}
	for _, h := range hosts {
		out[string(h.Template)] = append(out[string(h.Template)], h)
	}
	return out
}

func (s *Server) poolState(ctx context.Context, tx pgx.Tx, pl poolRow, st *poolState) error {
	// Runs waiting for a host in this pool. A tenant pool serves its
	// tenant; a platform pool anyone whose Runs name it (and who has no
	// pool of their own by that name).
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r
		WHERE r.state = 'provisioning' AND NOT r.cancel_requested
		  AND coalesce(r.spec->'placement'->>'pool', 'default') = $1
		  AND CASE WHEN $2::text IS NULL
		           THEN NOT EXISTS (SELECT 1 FROM pools o WHERE o.tenant_id = r.tenant_id AND o.name = $1 AND NOT o.retired)
		           ELSE r.tenant_id = $2 END`, pl.Name, pl.TenantID).Scan(&st.demand); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT h.id, h.provider_id, coalesce(h.launch_template, $5), h.state, h.draining,
			EXISTS (SELECT 1 FROM placements p WHERE p.host_id = h.id AND p.state IN `+livePlacementStates+`),
			EXISTS (SELECT 1 FROM blobs b WHERE b.host_id = h.id AND b.location = 'host'),
			coalesce(h.last_placement_ended_at, h.registered_at, h.created_at) < now() - $3::interval,
			h.provision_requested_at < now() - $4::interval
		FROM hosts h
		WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '') AND h.provider_id IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.Name, pl.TenantID, interval(s.cfg.ScaleDownAfter), interval(s.cfg.LaunchTimeout), pl.Template)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h hostRef
		var state string
		var draining, busy, pending, idleLong, launchLong bool
		if err := rows.Scan(&h.ID, &h.ProviderID, &h.Template, &state, &draining, &busy, &pending, &idleLong, &launchLong); err != nil {
			return err
		}
		switch {
		case state == "provisioning" && launchLong:
			h.Reason = "never registered"
			st.terminate = append(st.terminate, h)
			continue
		case state == "lost":
			// Its runner is gone; its Runs were written off. The instance
			// may still run (and bill): terminate it; a replacement comes
			// from the usual scale-up.
			h.Reason = "lost: terminated"
			st.terminate = append(st.terminate, h)
			continue
		case draining || state == "draining":
			if !busy && !pending {
				h.Reason = "drained: terminated"
				st.terminate = append(st.terminate, h)
			}
			st.existing = append(st.existing, h)
			continue // not counted: on its way out
		case state == "provisioning":
			st.provisioning++
		case state == "ready" && !busy:
			st.idle++
			if idleLong {
				st.idleExpired = append(st.idleExpired, h.ID)
			}
		}
		st.total++
		st.existing = append(st.existing, h)
	}
	return rows.Err()
}

// launch asks the provider for one host. Its row exists first (state
// provisioning, with a one-use host token, and a provider id reserved
// before the call: a luxd that stops right after the launch still knows
// the host, and terminates it if it never registers).
func (s *Server) launch(ctx context.Context, prov Provider, pl poolRow) error {
	hostID := ids.New(ids.Host)
	name := fmt.Sprintf("%s-%s", pl.Name, hostID[len(hostID)-8:])
	token := ids.Secret("luxh")
	tokenID := ids.New(ids.HostToken)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if pl.TenantID != nil {
			var n, quota int
			if err := tx.QueryRow(ctx, `SELECT count(*), coalesce((SELECT max_hosts FROM tenants WHERE id = $1), 0)
				FROM hosts WHERE tenant_id = $1 AND state IN ('provisioning', 'ready', 'draining', 'lost')`, *pl.TenantID).Scan(&n, &quota); err != nil {
				return err
			}
			if quota > 0 && n >= quota {
				return errQuotaFull
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ($1, $2, $3, $4)`,
			tokenID, pl.TenantID, pl.Name, ids.Hash(token)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool, token_id, name, state, provision_requested_at,
				provider_id, launch_template)
			VALUES ($1, $2, $3, $4, $5, 'provisioning', now(), $6, $7)`,
			hostID, pl.TenantID, pl.Name, tokenID, name, "pending:"+hostID, pl.Template)
		return err
	})
	if errors.Is(err, errQuotaFull) {
		return nil // at the tenant's host quota: Runs wait
	}
	if err != nil {
		return err
	}
	env := map[string]string{"LUX_URL": s.cfg.PublicURL, "LUX_HOST_TOKEN": token, "LUX_HOST_NAME": name}
	pid, err := prov.Launch(ctx, pl.Name, name, pl.Template, env)
	if err != nil {
		s.markTerminated(ctx, hostID, "launch failed: "+truncate(err.Error(), 200))
		return fmt.Errorf("launch in %s: %w", pl.Name, err)
	}
	s.log.Info("host launched", "pool", pl.Name, "host", name, "providerId", pid)
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET provider_id = $2 WHERE id = $1`, hostID, pid)
		return err
	})
}

var errQuotaFull = errors.New("host quota reached")

// drainForScaleDown takes an idle host out of service; the next pass
// terminates it once it has nothing left to upload.
// Only if it is still idle: the scheduler may have placed a Run on it
// since the pool was counted.
func (s *Server) drainForScaleDown(ctx context.Context, hostID string) (bool, error) {
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		hosts, err = s.drainHosts(ctx, tx, "idle: scaling down", `id = $1 AND state = 'ready'
			AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.host_id = hosts.id AND p.state IN `+livePlacementStates+`)`, hostID)
		return err
	})
	s.notifyAll(hosts)
	return len(hosts) > 0, err
}

func (s *Server) markTerminated(ctx context.Context, hostID, reason string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET state = 'terminated', state_reason = $2,
				terminate_requested_at = coalesce(terminate_requested_at, now()), terminated_at = now()
			WHERE id = $1`, hostID, reason)
		if err != nil {
			return err
		}
		if err := hostsGone(ctx, tx, []string{hostID}); err != nil {
			return err
		}
		// Its host token was made for it alone (launch): spent.
		_, err = tx.Exec(ctx, `UPDATE host_tokens SET revoked_at = now()
			WHERE id = (SELECT token_id FROM hosts WHERE id = $1 AND provision_requested_at IS NOT NULL)`, hostID)
		return err
	})
	if err != nil {
		s.log.Warn("mark terminated", "host", hostID, "err", err)
		return
	}
	s.hub.Disconnect(hostID)
}

// hostsGone: the hosts' copies of snapshots are gone with them; uploaded
// snapshots stay available.
func hostsGone(ctx context.Context, tx pgx.Tx, hosts []string) error {
	_, err := tx.Exec(ctx, `UPDATE snapshots SET available = available AND uploaded, host_copy = false
		WHERE host_id = ANY($1)`, hosts)
	return err
}
