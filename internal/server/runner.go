package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func (s *Server) runnerRoutes(mux *http.ServeMux) {
	mux.Handle("GET /runner/ws", s.wrap(s.serveRunnerWS))
	mux.Handle("POST /runner/poll", s.wrap(s.servePoll))
	mux.Handle("PUT /runner/blobs/{id}", s.wrap(s.serveBlobUpload))
	mux.Handle("GET /runner/blobs/{id}", s.wrap(s.serveRunnerBlobDownload))
}

// registerHost records a runner's hello: creates the host row on first
// contact, refreshes it otherwise, and reconciles which placements it should
// still be running.
func (s *Server) registerHost(ctx context.Context, tok *hostToken, h proto.Hello) (proto.Welcome, error) {
	if h.ProtocolVersion != proto.Version {
		return proto.Welcome{}, fmt.Errorf("incompatible runner: protocol %d, luxd speaks %d", h.ProtocolVersion, proto.Version)
	}
	if h.Name == "" {
		return proto.Welcome{}, errors.New("runner has no name")
	}
	labels := map[string]string{}
	for k, v := range tok.Labels {
		labels[k] = v
	}
	for k, v := range h.Labels {
		labels[k] = v
	}
	labels["arch"] = h.Arch
	labels["name"] = h.Name

	var w proto.Welcome
	w.LeaseSeconds = s.cfg.LeaseDuration.Seconds()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var hostID string
		err := tx.QueryRow(ctx, `SELECT id FROM hosts
			WHERE coalesce(tenant_id, '') = coalesce($1, '') AND name = $2 AND state <> 'terminated'`,
			tok.TenantID, h.Name).Scan(&hostID)
		caches := map[string]any{"images": h.Images, "gitMirrors": h.GitMirrors}
		versions := map[string]any{"runner": h.RunnerVersion, "shim": h.ShimVersion, "podman": h.PodmanVersion, "protocol": h.ProtocolVersion}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// A provisioned host may already have a row, created when luxd
			// asked the provider for it; match it by provider id.
			if h.ProviderID != "" {
				err = tx.QueryRow(ctx, `UPDATE hosts SET name = $2
					WHERE provider_id = $1 AND state = 'provisioning' RETURNING id`, h.ProviderID, h.Name).Scan(&hostID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			if hostID == "" {
				hostID = ids.New(ids.Host)
				if tok.TenantID != nil {
					var n, max int
					if err := tx.QueryRow(ctx, `SELECT count(*), coalesce((SELECT max_hosts FROM tenants WHERE id = $1), 0)
						FROM hosts WHERE tenant_id = $1 AND state IN ('provisioning', 'ready', 'draining')`, *tok.TenantID).Scan(&n, &max); err != nil {
						return err
					}
					if max > 0 && n >= max {
						return fmt.Errorf("tenant host quota reached (%d)", max)
					}
				}
				if _, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool, token_id, name, state)
					VALUES ($1, $2, $3, $4, $5, 'ready')`, hostID, tok.TenantID, tok.Pool, tok.ID, h.Name); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE hosts SET
				state = CASE WHEN draining THEN 'draining' ELSE 'ready' END,
				state_reason = '', labels = $2, arch = $3, capacity = $4, versions = $5, caches = $6,
				local_snapshots = $7, provider_id = coalesce(nullif($8, ''), provider_id),
				registered_at = coalesce(registered_at, now()),
				provisioned_at = coalesce(provisioned_at, now()),
				last_heartbeat = now(), lost_at = NULL
			WHERE id = $1`,
			hostID, labels, h.Arch, h.Capacity, versions, caches, nonNil(h.LocalSnapshots), h.ProviderID)
		if err != nil {
			return err
		}
		w.HostID = hostID

		// What luxd thinks is live here. Anything the runner has beyond this
		// is stale (its Run moved on) and it must stop it.
		rows, err := tx.Query(ctx, `SELECT run_id, epoch, state FROM placements
			WHERE host_id = $1 AND state IN ('assigned', 'starting', 'running', 'stopping')`, hostID)
		if err != nil {
			return err
		}
		live := map[string]int{}
		for rows.Next() {
			var lp proto.LivePlacement
			if err := rows.Scan(&lp.RunID, &lp.Epoch, &lp.State); err != nil {
				return err
			}
			w.Live = append(w.Live, lp)
			live[lp.RunID] = lp.Epoch
		}
		rows.Close()

		// Placements the runner no longer has: it lost them while
		// disconnected (e.g. the runner restarted and the container was gone).
		have := map[string]int{}
		for _, lp := range h.Live {
			have[lp.RunID] = lp.Epoch
		}
		for runID, epoch := range live {
			if e, ok := have[runID]; ok && e == epoch {
				continue
			}
			// Still assigned but not yet acked: the assign message is pending
			// and will be (re)delivered; not lost.
			var state string
			_ = tx.QueryRow(ctx, `SELECT state FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&state)
			if state == "assigned" {
				continue
			}
			if err := s.placementLost(ctx, tx, runID, epoch, "runner restarted without the container"); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.Kick()
	}
	return w, err
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// handleReport applies one runner report and returns the reply frame.
func (s *Server) handleReport(ctx context.Context, hostID string, f proto.Frame) proto.Frame {
	err := s.applyReport(ctx, hostID, f)
	if err != nil {
		var stale *staleError
		if errors.As(err, &stale) {
			return proto.Frame{Type: proto.MsgNack, ID: f.ID, RunID: f.RunID, Epoch: f.Epoch,
				Data: proto.Marshal(proto.Nack{Error: err.Error(), Stale: true})}
		}
		s.log.Warn("report failed", "type", f.Type, "run", f.RunID, "epoch", f.Epoch, "err", err)
		return proto.Frame{Type: proto.MsgNack, ID: f.ID, RunID: f.RunID, Epoch: f.Epoch,
			Data: proto.Marshal(proto.Nack{Error: err.Error()})}
	}
	return proto.Frame{Type: proto.MsgAck, ID: f.ID, RunID: f.RunID, Epoch: f.Epoch}
}

type staleError struct {
	runID          string
	epoch, current int
}

func (e *staleError) Error() string {
	return fmt.Sprintf("stale epoch %d for %s (current %d)", e.epoch, e.runID, e.current)
}

func (s *Server) applyReport(ctx context.Context, hostID string, f proto.Frame) error {
	switch f.Type {
	case proto.MsgHeartbeat:
		var hb proto.Heartbeat
		if err := json.Unmarshal(f.Data, &hb); err != nil {
			return err
		}
		return s.heartbeat(ctx, hostID, hb)
	case proto.MsgHello:
		return errors.New("hello after welcome")
	}

	var kicked bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Fencing: every report about a Run must carry the epoch of its
		// current placement, on this host.
		var tenantID, placementHost, placementState string
		var current int
		err := tx.QueryRow(ctx, `SELECT r.tenant_id, r.current_epoch, coalesce(p.host_id, ''), coalesce(p.state, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = $2
			WHERE r.id = $1 FOR UPDATE OF r`, f.RunID, f.Epoch).Scan(&tenantID, &current, &placementHost, &placementState)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("unknown run %s", f.RunID)
		}
		if err != nil {
			return err
		}
		// A placement luxd gave up on stays given up: a host that comes back
		// after being declared lost cannot revive it.
		if placementHost != hostID || placementState == "lost" {
			return &staleError{f.RunID, f.Epoch, current}
		}
		// Snapshots and uploads from an older placement on this host are
		// still wanted: they are that placement's final state. Status and
		// adapter events are not.
		if f.Epoch != current && f.Type != proto.MsgSnapshotDone {
			return &staleError{f.RunID, f.Epoch, current}
		}
		switch f.Type {
		case proto.MsgStatus:
			var st proto.Status
			if err := json.Unmarshal(f.Data, &st); err != nil {
				return err
			}
			kicked = true
			return s.applyStatus(ctx, tx, tenantID, f.RunID, f.Epoch, st)
		case proto.MsgAdapterEvent:
			var ev proto.AdapterEvent
			if err := json.Unmarshal(f.Data, &ev); err != nil {
				return err
			}
			return s.applyAdapterEvent(ctx, tx, tenantID, f.RunID, f.Epoch, ev)
		case proto.MsgSnapshotDone:
			var sd proto.SnapshotDone
			if err := json.Unmarshal(f.Data, &sd); err != nil {
				return err
			}
			kicked = true
			return s.applySnapshotDone(ctx, tx, tenantID, hostID, f.RunID, f.Epoch, current, sd)
		case proto.MsgRunEvent:
			var ev proto.RunEvent
			if err := json.Unmarshal(f.Data, &ev); err != nil {
				return err
			}
			return addEvent(ctx, tx, tenantID, f.RunID, f.Epoch, ev.Type, ev.Data)
		}
		return fmt.Errorf("unknown report type %q", f.Type)
	})
	if err == nil && kicked {
		s.Kick()
	}
	return err
}

func (s *Server) heartbeat(ctx context.Context, hostID string, hb proto.Heartbeat) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = now(),
			state = CASE WHEN state = 'lost' THEN (CASE WHEN draining THEN 'draining' ELSE 'ready' END) ELSE state END
			WHERE id = $1`, hostID); err != nil {
			return err
		}
		for _, l := range hb.Leases {
			if _, err := tx.Exec(ctx, `UPDATE placements SET lease_expires_at = now() + $4::interval
				WHERE run_id = $1 AND epoch = $2 AND host_id = $3 AND state IN ('assigned', 'starting', 'running', 'stopping')`,
				l.RunID, l.Epoch, hostID, fmt.Sprintf("%f seconds", s.cfg.LeaseDuration.Seconds())); err != nil {
				return err
			}
			if l.Usage != nil {
				if err := recordUsage(ctx, tx, l.RunID, l.Epoch, l.Usage); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// recordUsage raises the placement's peaks; values only ever grow.
func recordUsage(ctx context.Context, tx pgx.Tx, runID string, epoch int, u *proto.Usage) error {
	_, err := tx.Exec(ctx, `UPDATE placements SET
			peak_memory_bytes = greatest(peak_memory_bytes, nullif($3, 0)),
			peak_disk_bytes   = greatest(peak_disk_bytes, nullif($4, 0)),
			peak_pids         = greatest(peak_pids, nullif($5, 0)),
			cpu_seconds       = greatest(cpu_seconds, nullif($6, 0)),
			net_rx_bytes      = greatest(net_rx_bytes, nullif($7, 0)),
			net_tx_bytes      = greatest(net_tx_bytes, nullif($8, 0))
		WHERE run_id = $1 AND epoch = $2`,
		runID, epoch, u.PeakMemoryBytes, u.PeakDiskBytes, u.PeakPids, u.CPUSeconds, u.NetRxBytes, u.NetTxBytes)
	return err
}

// servePoll is the fallback transport: acks and reports in, pending
// messages and replies out. No live output in this mode.
func (s *Server) servePoll(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	var req struct {
		proto.Poll
		Hello *proto.Hello `json:"hello,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var hostID string
	resp := proto.PollResponse{}
	if req.Hello != nil {
		welcome, err := s.registerHost(r.Context(), tok, *req.Hello)
		if err != nil {
			return errf(http.StatusBadRequest, "rejected", "%v", err)
		}
		hostID = welcome.HostID
		resp.Replies = append(resp.Replies, proto.Frame{Type: proto.MsgWelcome, Data: proto.Marshal(welcome)})
	} else {
		err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT id FROM hosts WHERE token_id = $1 AND name = $2 AND state <> 'terminated'`,
				tok.ID, r.URL.Query().Get("name")).Scan(&hostID)
		})
		if err != nil {
			return errf(http.StatusConflict, "hello_required", "send hello first")
		}
	}
	s.hub.polled(hostID)
	for _, id := range req.Acks {
		if err := s.ackMessage(r.Context(), hostID, id); err != nil {
			return err
		}
	}
	for _, f := range req.Reports {
		resp.Replies = append(resp.Replies, s.handleReport(r.Context(), hostID, f))
	}
	msgs, err := s.pendingMessages(r.Context(), hostID)
	if err != nil {
		return err
	}
	resp.Messages = msgs
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func addEvent(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, typ string, data any) error {
	if data == nil || isNilMap(data) {
		data = map[string]any{}
	}
	var ep *int
	if epoch > 0 {
		ep = &epoch
	}
	_, err := tx.Exec(ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES ($1, $2, $3, $4, $5)`,
		tenantID, runID, ep, typ, data)
	return err
}

func isNilMap(v any) bool {
	m, ok := v.(map[string]any)
	return ok && m == nil
}

func msToTime(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}
