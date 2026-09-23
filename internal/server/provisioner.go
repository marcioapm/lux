package server

import (
	"context"
	"encoding/json"
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
	// Alive reports which of the given hosts still exist (not terminated).
	Alive(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]bool, error)
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
	var pools []poolRow
	locked := false
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(provisionLock)).Scan(&locked); err != nil || !locked {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, name, provider, tenant_id, template, min_hosts, max_hosts, warm_hosts, retired
			FROM pools WHERE provider <> 'static'`)
		if err != nil {
			return err
		}
		pools, err = pgx.CollectRows(rows, pgx.RowToStructByPos[poolRow])
		return err
	})
	if err != nil || !locked {
		return err
	}
	for _, pl := range pools {
		prov := s.cfg.Providers[pl.Provider]
		if prov == nil {
			continue
		}
		if err := s.reconcilePool(ctx, prov, pl); err != nil {
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
	// Draining hosts that are done (no live placements, nothing to upload).
	drained []hostRef
	// Provisioning hosts past the launch timeout.
	stuck []hostRef
	// Hosts in the pool the provider should still have.
	existing []hostRef
}

type hostRef struct{ ID, ProviderID string }

func (s *Server) reconcilePool(ctx context.Context, prov Provider, pl poolRow) error {
	var st poolState
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.poolState(ctx, tx, pl, &st)
	})
	if err != nil {
		return err
	}

	// Hosts the provider no longer has (terminated behind our back, or a
	// launch that died): their rows go, and their Runs are lost by the
	// usual heartbeat path.
	if len(st.existing) > 0 {
		pids := make([]string, len(st.existing))
		for i, h := range st.existing {
			pids[i] = h.ProviderID
		}
		alive, err := prov.Alive(ctx, pl.Template, pids)
		if err == nil {
			for _, h := range st.existing {
				if !alive[h.ProviderID] {
					s.markTerminated(ctx, h.ID, "the provider no longer has this host")
					st.total--
				}
			}
		}
	}

	// Scale down first: drain idle hosts beyond the floor, terminate the
	// drained and the stuck.
	floor := max(pl.Min, pl.Warm+st.demand)
	for _, id := range st.idleExpired {
		if st.total <= floor {
			break
		}
		if err := s.drainForScaleDown(ctx, id); err != nil {
			return err
		}
		st.total--
		st.idle--
	}
	for _, h := range append(st.drained, st.stuck...) {
		if err := prov.Terminate(ctx, pl.Template, h.ProviderID); err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			continue
		}
		s.markTerminated(ctx, h.ID, "terminated")
	}

	// Scale up: enough for what waits plus the warm hosts, at least the
	// minimum, never past the maximum.
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

func (s *Server) poolState(ctx context.Context, tx pgx.Tx, pl poolRow, st *poolState) error {
	// Runs waiting for a host in this pool. A tenant pool serves its
	// tenant; a platform pool anyone whose Runs name it (and who has no
	// pool of their own by that name). A retired pool serves no one.
	if pl.Retired {
		st.demand = 0
	} else if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r
		WHERE r.state = 'provisioning' AND NOT r.cancel_requested
		  AND coalesce(r.spec->'placement'->>'pool', 'default') = $1
		  AND CASE WHEN $2::text IS NULL
		           THEN NOT EXISTS (SELECT 1 FROM pools o WHERE o.tenant_id = r.tenant_id AND o.name = $1)
		           ELSE r.tenant_id = $2 END`, pl.Name, pl.TenantID).Scan(&st.demand); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT h.id, coalesce(h.provider_id, ''), h.state, h.draining,
			EXISTS (SELECT 1 FROM placements p WHERE p.host_id = h.id AND p.state IN `+livePlacementStates+`),
			EXISTS (SELECT 1 FROM blobs b WHERE b.host_id = h.id AND b.location = 'host'),
			coalesce(h.last_placement_ended_at, h.registered_at, h.created_at) < now() - $3::interval,
			h.provision_requested_at < now() - $4::interval
		FROM hosts h
		WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '') AND h.provider_id IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.Name, pl.TenantID, interval(s.cfg.ScaleDownAfter), interval(s.cfg.LaunchTimeout))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h hostRef
		var state string
		var draining, busy, pending, idleLong, launchLong bool
		if err := rows.Scan(&h.ID, &h.ProviderID, &state, &draining, &busy, &pending, &idleLong, &launchLong); err != nil {
			return err
		}
		switch {
		case state == "provisioning" && launchLong:
			st.stuck = append(st.stuck, h)
			continue
		case state == "provisioning":
			st.provisioning++
		case draining || state == "draining":
			if !busy && !pending {
				st.drained = append(st.drained, h)
			}
			st.existing = append(st.existing, h)
			continue // not counted: on its way out
		case state == "ready" && !busy:
			st.idle++
			if idleLong {
				st.idleExpired = append(st.idleExpired, h.ID)
			}
		}
		if state != "lost" {
			st.total++
		}
		st.existing = append(st.existing, h)
	}
	return rows.Err()
}

// launch asks the provider for one host. Its row exists first (state
// provisioning, with a one-use host token), so a runner that registers
// quickly finds it by provider id.
func (s *Server) launch(ctx context.Context, prov Provider, pl poolRow) error {
	hostID := ids.New(ids.Host)
	name := fmt.Sprintf("%s-%s", pl.Name, hostID[len(hostID)-8:])
	token := ids.Secret("luxh")
	tokenID := ids.New(ids.HostToken)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ($1, $2, $3, $4)`,
			tokenID, pl.TenantID, pl.Name, ids.Hash(token)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool, token_id, name, state, provision_requested_at)
			VALUES ($1, $2, $3, $4, $5, 'provisioning', now())`, hostID, pl.TenantID, pl.Name, tokenID, name)
		return err
	})
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

// drainForScaleDown takes an idle host out of service; the next pass
// terminates it once it has nothing left to upload.
func (s *Server) drainForScaleDown(ctx context.Context, hostID string) error {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET draining = true, state = 'draining', state_reason = 'idle: scaling down',
			drain_requested_at = coalesce(drain_requested_at, now())
			WHERE id = $1 AND state = 'ready'`, hostID)
		if err != nil {
			return err
		}
		return s.drainPlacements(ctx, tx, hostID)
	})
	if err == nil {
		s.hub.Notify(hostID)
	}
	return err
}

func (s *Server) markTerminated(ctx context.Context, hostID, reason string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET state = 'terminated', state_reason = $2,
				terminate_requested_at = coalesce(terminate_requested_at, now()), terminated_at = now()
			WHERE id = $1`, hostID, reason)
		if err != nil {
			return err
		}
		// Its copies of snapshots go with it; uploaded ones stay available.
		_, err = tx.Exec(ctx, `UPDATE snapshots SET available = available AND uploaded, host_copy = false WHERE host_id = $1`, hostID)
		if err != nil {
			return err
		}
		// Its one-use host token is spent.
		_, err = tx.Exec(ctx, `UPDATE host_tokens SET revoked_at = now()
			WHERE id = (SELECT token_id FROM hosts WHERE id = $1) AND revoked_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM hosts o WHERE o.token_id = host_tokens.id AND o.id <> $1 AND o.state <> 'terminated')`, hostID)
		return err
	})
	if err != nil {
		s.log.Warn("mark terminated", "host", hostID, "err", err)
	}
	s.hub.Disconnect(hostID)
}
