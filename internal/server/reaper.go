package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// reaperLoop enforces time: expired leases, lost hosts, Run timeouts, and
// retention of the blobs of finished Runs.
func (s *Server) reaperLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, f := range []func(context.Context) error{s.reapLeases, s.reapHosts, s.reapTimeouts, s.reapRetention} {
			if err := f(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("reaper", "err", err)
			}
		}
	}
}

// reapLeases: a placement whose lease expired is lost.
func (s *Server) reapLeases(ctx context.Context) error {
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		expired, err := livePlacements(ctx, tx, "p.lease_expires_at < now()")
		if err != nil {
			return err
		}
		for _, p := range expired {
			if err := s.placementLost(ctx, tx, p.RunID, p.Epoch, "lease expired: host stopped heartbeating"); err != nil {
				return err
			}
		}
		n = len(expired)
		return nil
	})
	if n > 0 {
		s.Kick()
	}
	return err
}

// reapHosts: a host without heartbeats is lost, and with it its live
// placements and the snapshots only it held.
func (s *Server) reapHosts(ctx context.Context) error {
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE hosts SET state = 'lost', lost_at = now(), state_reason = 'missed heartbeats'
			WHERE state IN ('ready', 'draining') AND last_heartbeat < now() - $1::interval
			RETURNING id`, interval(s.cfg.LeaseDuration))
		if err != nil {
			return err
		}
		lost, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(lost) == 0 {
			return err
		}
		n = len(lost)
		s.log.Warn("hosts lost", "hosts", lost)
		if _, err := tx.Exec(ctx, `UPDATE snapshots SET available = available AND uploaded, host_copy = false
			WHERE host_id = ANY($1)`, lost); err != nil {
			return err
		}
		// Their live placements go with them, whatever their leases say: a
		// fresh assignment's lease is generous (image pulls are slow), but a
		// host that stopped heartbeating is not pulling anything.
		live, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", lost)
		if err != nil {
			return err
		}
		for _, p := range live {
			if err := s.placementLost(ctx, tx, p.RunID, p.Epoch, "host lost: missed heartbeats"); err != nil {
				return err
			}
		}
		return nil
	})
	if n > 0 {
		s.Kick()
	}
	return err
}

// reapTimeouts stops Runs that exceeded their wall-clock timeout, counted
// across all placements from first start.
func (s *Server) reapTimeouts(ctx context.Context) error {
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.id, r.tenant_id, r.current_epoch FROM runs r
			WHERE r.state IN ('starting', 'running') AND r.first_started_at IS NOT NULL
			  AND r.first_started_at + ((r.spec->>'timeout')::interval) < now()
			  AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.stop_requested_at IS NOT NULL)
			LIMIT 50`)
		if err != nil {
			return err
		}
		type rr struct {
			id, tenant string
			epoch      int
		}
		var due []rr
		for rows.Next() {
			var r rr
			if err := rows.Scan(&r.id, &r.tenant, &r.epoch); err != nil {
				rows.Close()
				return err
			}
			due = append(due, r)
		}
		rows.Close()
		for _, r := range due {
			h, err := s.requestStop(ctx, tx, r.tenant, r.id, "timeout")
			if err != nil {
				return err
			}
			if h != "" {
				hosts = append(hosts, h)
			}
		}
		return nil
	})
	for _, h := range hosts {
		s.hub.Notify(h)
	}
	return err
}

// requestStop asks the current placement to wind down. Returns the host to
// notify, or "" when there is no live placement.
func (s *Server) requestStop(ctx context.Context, tx pgx.Tx, tenantID, runID, reason string) (string, error) {
	var hostID string
	var epoch int
	err := tx.QueryRow(ctx, `UPDATE placements p SET stop_requested_at = coalesce(stop_requested_at, now()),
			stop_reason = CASE WHEN stop_reason = '' OR $2 = 'cancel' THEN $2 ELSE stop_reason END
		FROM runs r
		WHERE r.id = $1 AND p.run_id = r.id AND p.epoch = r.current_epoch
		  AND p.state IN `+livePlacementStates+`
		RETURNING p.host_id, p.epoch`, runID, reason).Scan(&hostID, &epoch)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	typ := proto.MsgStop
	if reason == "cancel" {
		typ = proto.MsgCancel
	}
	if err := enqueue(ctx, tx, hostID, runID, epoch, typ, proto.StopRequest{Reason: reason}); err != nil {
		return "", err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		return "", err
	}
	if state != StateStopping {
		if err := setRunState(ctx, tx, tenantID, runID, StateStopping, reason, epoch); err != nil {
			return "", err
		}
	}
	return hostID, nil
}

// reapRetention deletes the blobs of Runs that finished longer ago than
// their tenant's retention.
func (s *Server) reapRetention(ctx context.Context) error {
	type b struct{ id, key string }
	var due []b
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT bl.id, coalesce(bl.s3_key, '') FROM blobs bl
			JOIN runs r ON r.id = bl.run_id JOIN tenants t ON t.id = r.tenant_id
			WHERE r.finished_at IS NOT NULL AND r.finished_at < now() - make_interval(days => t.retention_days)
			  AND bl.location <> 'deleted'
			LIMIT 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x b
			if err := rows.Scan(&x.id, &x.key); err != nil {
				return err
			}
			due = append(due, x)
		}
		return rows.Err()
	})
	if err != nil || len(due) == 0 {
		return err
	}
	var deleted []string
	for _, x := range due {
		if x.key != "" {
			if err := s.blobs.Delete(ctx, x.key); err != nil {
				break
			}
		}
		deleted = append(deleted, x.id)
	}
	if len(deleted) == 0 {
		return nil
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE blobs SET location = 'deleted', deleted_at = now() WHERE id = ANY($1)`, deleted)
		return err
	})
}
