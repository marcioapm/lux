package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("POST /v1/runs", s.withKey("run", s.submitRun))
	mux.Handle("GET /v1/runs", s.withKey("read", s.listRuns))
	mux.Handle("GET /v1/runs/{id}", s.withKey("read", s.getRun))
	mux.Handle("GET /v1/runs/{id}/events", s.withKey("read", s.listEvents))
	mux.Handle("GET /v1/runs/{id}/output", s.withKey("read", s.serveOutput))
	mux.Handle("POST /v1/runs/{id}/input", s.withKey("run", s.postInput))
	mux.Handle("POST /v1/runs/{id}/stop", s.withKey("run", s.stopRun))
	mux.Handle("POST /v1/runs/{id}/resume", s.withKey("run", s.resumeRun))
	mux.Handle("POST /v1/runs/{id}/cancel", s.withKey("run", s.cancelRun))
	mux.Handle("POST /v1/runs/{id}/push", s.withKey("run", s.pushRun))
	mux.Handle("GET /v1/runs/{id}/snapshots", s.withKey("read", s.listSnapshots))
	mux.Handle("GET /v1/runs/{id}/artifacts", s.withKey("read", s.listArtifacts))
	mux.Handle("GET /v1/artifacts/{aid}", s.withKey("read", s.downloadArtifact))
	mux.Handle("GET /v1/runs/{id}/exec", s.withKey("run", s.serveExec))
	mux.Handle("GET /v1/runs/{id}/attach", s.withKey("run", s.serveAttach))
	mux.Handle("GET /v1/runs/{id}/ports/{name}", s.withKey("run", s.servePort))
	mux.Handle("GET /v1/hosts", s.withKey("read", s.listHosts))
	mux.Handle("POST /v1/hosts/{id}/drain", s.withKey("admin", s.drainHost))
	mux.Handle("GET /v1/pools", s.withKey("read", s.listPools))
	mux.Handle("POST /v1/pools", s.withKey("admin", s.putPool))
}

// Run is the API representation of a Run.
type Run struct {
	ID          string            `json:"id"`
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels"`
	State       string            `json:"state"`
	StateReason string            `json:"stateReason,omitempty"`
	Activity    string            `json:"activity,omitempty"`
	ExitCode    *int              `json:"exitCode,omitempty"`
	Epoch       int               `json:"epoch"`
	SessionID   string            `json:"sessionId,omitempty"`
	SnapshotID  *string           `json:"snapshotId,omitempty"`
	Host        string            `json:"host,omitempty"`
	Spec        spec.RunSpec      `json:"spec"`
	Secrets     []spec.SecretRef  `json:"secrets"`
	CreatedAt   time.Time         `json:"createdAt"`
	ScheduledAt *time.Time        `json:"firstScheduledAt,omitempty"`
	StartedAt   *time.Time        `json:"firstStartedAt,omitempty"`
	FinishedAt  *time.Time        `json:"finishedAt,omitempty"`
	Placements  []Placement       `json:"placements,omitempty"`
	Usage       *RunUsage         `json:"usage,omitempty"`
}

type Placement struct {
	Epoch              int        `json:"epoch"`
	Host               string     `json:"host"`
	HostName           string     `json:"hostName"`
	State              string     `json:"state"`
	ExitCode           *int       `json:"exitCode,omitempty"`
	ExitReason         string     `json:"exitReason,omitempty"`
	StopReason         string     `json:"stopReason,omitempty"`
	AssignedAt         time.Time  `json:"assignedAt"`
	AcceptedAt         *time.Time `json:"acceptedAt,omitempty"`
	ImageReadyAt       *time.Time `json:"imageReadyAt,omitempty"`
	VolumesRestoredAt  *time.Time `json:"volumesRestoredAt,omitempty"`
	ContainerStartedAt *time.Time `json:"containerStartedAt,omitempty"`
	WorkloadStartedAt  *time.Time `json:"workloadStartedAt,omitempty"`
	StopRequestedAt    *time.Time `json:"stopRequestedAt,omitempty"`
	ExitedAt           *time.Time `json:"exitedAt,omitempty"`
	SnapshotDoneAt     *time.Time `json:"snapshotDoneAt,omitempty"`
	UploadedAt         *time.Time `json:"uploadedAt,omitempty"`
	PeakMemoryBytes    *int64     `json:"peakMemoryBytes,omitempty"`
	PeakDiskBytes      *int64     `json:"peakDiskBytes,omitempty"`
	PeakPids           *int       `json:"peakPids,omitempty"`
	CPUSeconds         *float64   `json:"cpuSeconds,omitempty"`
	NetRxBytes         *int64     `json:"netRxBytes,omitempty"`
	NetTxBytes         *int64     `json:"netTxBytes,omitempty"`
	SnapshotBytes      *int64     `json:"snapshotBytes,omitempty"`
}

// RunUsage rolls placements up: peaks are maxima, totals are sums.
type RunUsage struct {
	PeakMemoryBytes int64   `json:"peakMemoryBytes"`
	PeakDiskBytes   int64   `json:"peakDiskBytes"`
	PeakPids        int     `json:"peakPids"`
	CPUSeconds      float64 `json:"cpuSeconds"`
	NetRxBytes      int64   `json:"netRxBytes"`
	NetTxBytes      int64   `json:"netTxBytes"`
	Placements      int     `json:"placements"`
	// Seconds between being requested and the first workload start.
	QueueSeconds *float64 `json:"queueSeconds,omitempty"`
}

const runColumns = `r.id, r.name, r.labels, r.state, r.state_reason, r.activity, r.exit_code, r.current_epoch,
	r.session_id, r.snapshot_id, r.spec, r.secrets, r.created_at, r.first_scheduled_at, r.first_started_at, r.finished_at,
	coalesce((SELECT h.name FROM placements p JOIN hosts h ON h.id = p.host_id WHERE p.run_id = r.id AND p.epoch = r.current_epoch), '')`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Name, &r.Labels, &r.State, &r.StateReason, &r.Activity, &r.ExitCode, &r.Epoch,
		&r.SessionID, &r.SnapshotID, &r.Spec, &r.Secrets, &r.CreatedAt, &r.ScheduledAt, &r.StartedAt, &r.FinishedAt, &r.Host)
	return &r, err
}

type submitRequest struct {
	spec.RunSpec
}

func (s *Server) submitRun(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	var sp spec.RunSpec
	if err := readJSON(r, &sp); err != nil {
		return err
	}
	if err := sp.Normalize(); err != nil {
		var ve *spec.ValidationError
		if errors.As(err, &ve) {
			he := errf(http.StatusUnprocessableEntity, "invalid_spec", "%s", err.Error())
			he.Details = ve.Problems
			return he
		}
		return err
	}
	for _, sec := range sp.Secrets {
		if sec.Value == "" {
			return errf(http.StatusUnprocessableEntity, "invalid_spec", "secret %q has no value", sec.Name)
		}
	}
	stored, refs, values := sp.SplitSecrets()
	idem := r.Header.Get("Idempotency-Key")

	run := &Run{}
	created := false
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if idem != "" {
			existing, err := scanRun(tx.QueryRow(r.Context(), `SELECT `+runColumns+` FROM runs r WHERE idempotency_key = $1`, idem))
			if err == nil {
				run = existing
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if err := checkRunQuota(r.Context(), tx, p.TenantID); err != nil {
			return err
		}
		id := ids.New(ids.Run)
		var idemArg *string
		if idem != "" {
			idemArg = &idem
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO runs (id, tenant_id, name, labels, spec, secrets, state, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, 'submitted', $7)`,
			id, p.TenantID, sp.Name, nonNilMap(sp.Labels), stored, refs, idemArg)
		if err != nil {
			return err
		}
		if err := addEvent(r.Context(), tx, p.TenantID, id, 0, "submitted", map[string]any{"by": p.KeyID}); err != nil {
			return err
		}
		created = true
		run, err = scanRun(tx.QueryRow(r.Context(), `SELECT `+runColumns+` FROM runs r WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return err
	}
	if created {
		if len(values) > 0 {
			s.secrets.put(run.ID, values)
		}
		s.Kick()
		writeJSON(w, http.StatusCreated, run)
		return nil
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

func checkRunQuota(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var max *int
	var n int
	if err := tx.QueryRow(ctx, `SELECT max_concurrent_runs, (SELECT count(*) FROM runs
			WHERE tenant_id = $1 AND state NOT IN ('succeeded', 'failed', 'cancelled', 'stopped', 'lost'))
		FROM tenants WHERE id = $1`, tenantID).Scan(&max, &n); err != nil {
		return err
	}
	if max != nil && n >= *max {
		return errf(http.StatusTooManyRequests, "quota_exceeded", "tenant has %d active runs (limit %d)", n, *max)
	}
	return nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	q := r.URL.Query()
	where := []string{"true"}
	args := []any{}
	if st := q.Get("state"); st != "" {
		args = append(args, strings.Split(st, ","))
		where = append(where, "r.state = ANY($"+strconv.Itoa(len(args))+")")
	}
	for _, l := range q["label"] {
		k, v, _ := strings.Cut(l, "=")
		args = append(args, map[string]string{k: v})
		where = append(where, "r.labels @> $"+strconv.Itoa(len(args)))
	}
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	runs := []*Run{}
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT `+runColumns+` FROM runs r WHERE `+strings.Join(where, " AND ")+
			` ORDER BY r.created_at DESC LIMIT `+strconv.Itoa(limit), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return err
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
	return nil
}

func (s *Server) loadRun(ctx context.Context, tenantID, id string, detail bool) (*Run, error) {
	var run *Run
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id = $1`, id))
		if err != nil || !detail {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.epoch, p.host_id, h.name, p.state, p.exit_code, p.exit_reason, p.stop_reason,
				p.created_at, p.accepted_at, p.image_ready_at, p.volumes_restored_at, p.container_started_at, p.workload_started_at,
				p.stop_requested_at, p.exited_at, p.snapshot_done_at, p.uploaded_at,
				p.peak_memory_bytes, p.peak_disk_bytes, p.peak_pids, p.cpu_seconds, p.net_rx_bytes, p.net_tx_bytes, p.snapshot_bytes
			FROM placements p JOIN hosts h ON h.id = p.host_id WHERE p.run_id = $1 ORDER BY p.epoch`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		u := &RunUsage{}
		for rows.Next() {
			var pl Placement
			if err := rows.Scan(&pl.Epoch, &pl.Host, &pl.HostName, &pl.State, &pl.ExitCode, &pl.ExitReason, &pl.StopReason,
				&pl.AssignedAt, &pl.AcceptedAt, &pl.ImageReadyAt, &pl.VolumesRestoredAt, &pl.ContainerStartedAt, &pl.WorkloadStartedAt,
				&pl.StopRequestedAt, &pl.ExitedAt, &pl.SnapshotDoneAt, &pl.UploadedAt,
				&pl.PeakMemoryBytes, &pl.PeakDiskBytes, &pl.PeakPids, &pl.CPUSeconds, &pl.NetRxBytes, &pl.NetTxBytes, &pl.SnapshotBytes); err != nil {
				return err
			}
			run.Placements = append(run.Placements, pl)
			u.Placements++
			if pl.PeakMemoryBytes != nil {
				u.PeakMemoryBytes = max(u.PeakMemoryBytes, *pl.PeakMemoryBytes)
			}
			if pl.PeakDiskBytes != nil {
				u.PeakDiskBytes = max(u.PeakDiskBytes, *pl.PeakDiskBytes)
			}
			if pl.PeakPids != nil {
				u.PeakPids = max(u.PeakPids, *pl.PeakPids)
			}
			if pl.CPUSeconds != nil {
				u.CPUSeconds += *pl.CPUSeconds
			}
			if pl.NetRxBytes != nil {
				u.NetRxBytes += *pl.NetRxBytes
			}
			if pl.NetTxBytes != nil {
				u.NetTxBytes += *pl.NetTxBytes
			}
		}
		if len(run.Placements) > 0 && run.Placements[0].WorkloadStartedAt != nil {
			q := run.Placements[0].WorkloadStartedAt.Sub(run.CreatedAt).Seconds()
			u.QueueSeconds = &q
		}
		run.Usage = u
		return rows.Err()
	})
	return run, err
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.loadRun(r.Context(), principal(r).TenantID, r.PathValue("id"), true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

type Event struct {
	ID    int64          `json:"id"`
	Epoch *int           `json:"epoch,omitempty"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
	Time  time.Time      `json:"time"`
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, err := s.events(r.Context(), p.TenantID, r.PathValue("id"), after)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
	return nil
}

func (s *Server) events(ctx context.Context, tenantID, runID string, after int64) ([]Event, error) {
	events := []Event{}
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, runID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errNotFound
		}
		rows, err := tx.Query(ctx, `SELECT id, epoch, type, data, created_at FROM run_events
			WHERE run_id = $1 AND id > $2 ORDER BY id LIMIT 1000`, runID, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ID, &e.Epoch, &e.Type, &e.Data, &e.Time); err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	return events, err
}

type inputRequest struct {
	Text      string `json:"text,omitempty"`
	Raw       []byte `json:"raw,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// postInput steers a live Run. The request id makes retries safe: the
// runner delivers each id once.
func (s *Server) postInput(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	var in inputRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Text == "" && len(in.Raw) == 0 && !in.Interrupt {
		return errf(http.StatusBadRequest, "bad_request", "text, raw or interrupt is required")
	}
	if in.RequestID == "" {
		in.RequestID = ids.New("in")
	}
	id := r.PathValue("id")
	var hostID string
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var epoch int
		if err := tx.QueryRow(r.Context(), `SELECT state, current_epoch FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state, &epoch); err != nil {
			return err
		}
		switch {
		case state == StateStopped || state == StateLost:
			return errf(http.StatusConflict, "not_running", "run is %s: use resume, which accepts an input", state)
		case terminal(state):
			return errf(http.StatusConflict, "not_running", "run is %s", state)
		case !live(state):
			return errf(http.StatusConflict, "not_running", "run is %s: not started yet", state)
		}
		if err := tx.QueryRow(r.Context(), `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, id, epoch).Scan(&hostID); err != nil {
			return err
		}
		msg := proto.Input{RequestID: in.RequestID, Text: in.Text, Raw: in.Raw, Interrupt: in.Interrupt}
		typ := proto.MsgInput
		if in.Interrupt && in.Text == "" && len(in.Raw) == 0 {
			typ = proto.MsgInterrupt
		}
		if err := s.systemEnqueue(r.Context(), hostID, id, epoch, typ, msg); err != nil {
			return err
		}
		return addEvent(r.Context(), tx, p.TenantID, id, epoch, "input", map[string]any{
			"requestId": in.RequestID, "interrupt": in.Interrupt, "text": in.Text, "rawBytes": len(in.Raw)})
	})
	if err != nil {
		return err
	}
	s.hub.Notify(hostID)
	writeJSON(w, http.StatusAccepted, map[string]any{"requestId": in.RequestID})
	return nil
}

// systemEnqueue writes a host message in its own system-scoped transaction:
// host_messages is not visible to tenant scopes.
func (s *Server) systemEnqueue(ctx context.Context, hostID, runID string, epoch int, typ string, payload any) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return enqueue(ctx, tx, hostID, runID, epoch, typ, payload)
	})
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) error {
	return s.stopOrCancel(w, r, "stop")
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) error {
	return s.stopOrCancel(w, r, "cancel")
}

func (s *Server) stopOrCancel(w http.ResponseWriter, r *http.Request, reason string) error {
	p := principal(r)
	id := r.PathValue("id")
	var hostID string
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(r.Context(), `SELECT state FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
			return err
		}
		if terminal(state) {
			return nil // idempotent
		}
		if reason == "cancel" {
			if _, err := tx.Exec(r.Context(), `UPDATE runs SET cancel_requested = true WHERE id = $1`, id); err != nil {
				return err
			}
		}
		if err := addEvent(r.Context(), tx, p.TenantID, id, 0, reason+".requested", map[string]any{"by": p.KeyID}); err != nil {
			return err
		}
		switch state {
		case StateSubmitted, StateResuming, StateProvisioning, StateStopped, StateLost:
			// Nothing running: settle it here.
			next := StateStopped
			if reason == "cancel" {
				next = StateCancelled
				s.secrets.drop(id)
			}
			if state == next {
				return nil
			}
			if state == StateLost && reason == "stop" {
				return nil
			}
			return setRunState(r.Context(), tx, p.TenantID, id, next, reason, 0)
		case StateStopping:
			if reason != "cancel" {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Live placement: ask its runner, in a system scope (placements are
	// tenant rows, host messages are not).
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		var tenant, state string
		if err := tx.QueryRow(r.Context(), `SELECT tenant_id, state FROM runs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, p.TenantID).Scan(&tenant, &state); err != nil {
			return err
		}
		if !live(state) {
			return nil
		}
		var err error
		hostID, err = s.requestStop(r.Context(), tx, tenant, id, reason)
		return err
	})
	if err != nil {
		return err
	}
	if hostID != "" {
		s.hub.Notify(hostID)
	}
	run, err := s.loadRun(r.Context(), p.TenantID, id, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, run)
	return nil
}

type resumeRequest struct {
	Secrets []spec.Secret `json:"secrets,omitempty"`
	Input   *struct {
		Text string `json:"text"`
	} `json:"input,omitempty"`
	// FromSnapshot resumes from an older snapshot (e.g. after lost).
	FromSnapshot string `json:"fromSnapshot,omitempty"`
}

// resumeRun puts a stopped, lost or failed Run back in the queue. Its
// secrets must be supplied again: luxd never kept them.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	id := r.PathValue("id")
	var req resumeRequest
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	values := map[string]string{}
	for _, sec := range req.Secrets {
		values[sec.Name] = sec.Value
	}
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var refs []spec.SecretRef
		if err := tx.QueryRow(r.Context(), `SELECT state, secrets FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state, &refs); err != nil {
			return err
		}
		switch state {
		case StateStopped, StateLost, StateFailed:
		case StateResuming:
			return nil // idempotent
		case StateCancelled, StateSucceeded:
			return errf(http.StatusConflict, "not_resumable", "run is %s", state)
		default:
			return errf(http.StatusConflict, "not_resumable", "run is %s: stop it first", state)
		}
		var missing []string
		for _, ref := range refs {
			if _, ok := values[ref.Name]; !ok {
				missing = append(missing, ref.Name)
			}
		}
		if len(missing) > 0 {
			he := errf(http.StatusUnprocessableEntity, "secrets_required", "resume needs the run's secrets again: %s", strings.Join(missing, ", "))
			he.Details = missing
			return he
		}
		// Rotation is allowed: record the new fingerprints.
		newRefs := make([]spec.SecretRef, 0, len(refs))
		for _, ref := range refs {
			newRefs = append(newRefs, spec.SecretRef{Name: ref.Name, Fingerprint: spec.Fingerprint(ref.Name, values[ref.Name])})
		}
		if _, err := tx.Exec(r.Context(), `UPDATE runs SET secrets = $2, cancel_requested = false WHERE id = $1`, id, newRefs); err != nil {
			return err
		}
		if req.FromSnapshot != "" {
			var ok bool
			if err := tx.QueryRow(r.Context(), `SELECT available FROM snapshots WHERE id = $1 AND run_id = $2`, req.FromSnapshot, id).Scan(&ok); err != nil {
				return errf(http.StatusNotFound, "not_found", "no snapshot %s for this run", req.FromSnapshot)
			}
			if !ok {
				return errf(http.StatusConflict, "snapshot_unavailable", "snapshot %s is no longer available", req.FromSnapshot)
			}
			if _, err := tx.Exec(r.Context(), `UPDATE runs SET snapshot_id = $2 WHERE id = $1`, id, req.FromSnapshot); err != nil {
				return err
			}
		}
		var in *proto.Input
		if req.Input != nil && req.Input.Text != "" {
			in = &proto.Input{RequestID: ids.New("in"), Text: req.Input.Text}
		}
		return s.requestResume(r.Context(), tx, p.TenantID, id, in, "resume requested")
	})
	if err != nil {
		return err
	}
	// Merge with any values still held, so names not in the spec (none) and
	// the full set are present.
	s.secrets.put(id, values)
	s.Kick()
	run, err := s.loadRun(r.Context(), p.TenantID, id, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, run)
	return nil
}

func (s *Server) pushRun(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	id := r.PathValue("id")
	var req struct {
		RequestID string `json:"requestId,omitempty"`
		Message   string `json:"message,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	if req.RequestID == "" {
		req.RequestID = ids.New("push")
	}
	var hostID string
	var epoch int
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var sp spec.RunSpec
		if err := tx.QueryRow(r.Context(), `SELECT state, current_epoch, spec FROM runs WHERE id = $1`, id).Scan(&state, &epoch, &sp); err != nil {
			return err
		}
		if sp.Git == nil || sp.Git.Push == nil {
			return errf(http.StatusConflict, "no_push", "the run's spec has no git.push branch")
		}
		if state != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: pushing needs a live placement", state)
		}
		if err := tx.QueryRow(r.Context(), `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, id, epoch).Scan(&hostID); err != nil {
			return err
		}
		return addEvent(r.Context(), tx, p.TenantID, id, epoch, "push.requested", map[string]any{"requestId": req.RequestID})
	})
	if err != nil {
		return err
	}
	if err := s.systemEnqueue(r.Context(), hostID, id, epoch, proto.MsgPush, req); err != nil {
		return err
	}
	s.hub.Notify(hostID)
	writeJSON(w, http.StatusAccepted, map[string]any{"requestId": req.RequestID})
	return nil
}

type Snapshot struct {
	ID        string         `json:"id"`
	Epoch     int            `json:"epoch"`
	Manifest  proto.Manifest `json:"manifest"`
	Available bool           `json:"available"`
	OnHost    string         `json:"onHost,omitempty"`
	Uploaded  bool           `json:"uploaded"`
	CreatedAt time.Time      `json:"createdAt"`
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	out := []Snapshot{}
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT s.id, s.epoch, s.manifest, s.available,
				CASE WHEN s.host_copy THEN coalesce(h.name, '') ELSE '' END, s.uploaded, s.created_at
			FROM snapshots s LEFT JOIN hosts h ON h.id = s.host_id WHERE s.run_id = $1 ORDER BY s.epoch`, r.PathValue("id"))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sn Snapshot
			if err := rows.Scan(&sn.ID, &sn.Epoch, &sn.Manifest, &sn.Available, &sn.OnHost, &sn.Uploaded, &sn.CreatedAt); err != nil {
				return err
			}
			out = append(out, sn)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
	return nil
}

type Host struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Pool          string                `json:"pool"`
	State         string                `json:"state"`
	StateReason   string                `json:"stateReason,omitempty"`
	Draining      bool                  `json:"draining"`
	Labels        map[string]string     `json:"labels"`
	Capacity      proto.Capacity        `json:"capacity"`
	Versions      map[string]any        `json:"versions"`
	Platform      bool                  `json:"platform"`
	LiveRuns      int                   `json:"liveRuns"`
	ProviderID    *string               `json:"providerId,omitempty"`
	LastHeartbeat *time.Time            `json:"lastHeartbeat,omitempty"`
	Times         map[string]*time.Time `json:"times"`
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	hosts := []Host{}
	// Tenants see their own hosts, and platform hosts in pools they can use.
	err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT h.id, h.name, h.pool, h.state, h.state_reason, h.draining, h.labels, h.capacity, h.versions,
				h.tenant_id IS NULL, (SELECT count(*) FROM placements pl WHERE pl.host_id = h.id AND pl.state IN ('assigned', 'starting', 'running', 'stopping')),
				h.provider_id, h.last_heartbeat,
				h.provision_requested_at, h.provisioned_at, h.registered_at, h.first_placement_at, h.last_placement_ended_at,
				h.drain_requested_at, h.terminate_requested_at, h.terminated_at, h.lost_at, h.created_at
			FROM hosts h WHERE (h.tenant_id = $1 OR h.tenant_id IS NULL) AND ($2 OR h.state <> 'terminated')
			ORDER BY h.name`, p.TenantID, r.URL.Query().Get("all") == "true")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Host
			var t [10]*time.Time
			if err := rows.Scan(&h.ID, &h.Name, &h.Pool, &h.State, &h.StateReason, &h.Draining, &h.Labels, &h.Capacity, &h.Versions,
				&h.Platform, &h.LiveRuns, &h.ProviderID, &h.LastHeartbeat,
				&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9]); err != nil {
				return err
			}
			h.Times = map[string]*time.Time{
				"provisionRequested": t[0], "provisioned": t[1], "registered": t[2], "firstPlacement": t[3],
				"lastPlacementEnded": t[4], "drainRequested": t[5], "terminateRequested": t[6], "terminated": t[7],
				"lost": t[8], "created": t[9],
			}
			hosts = append(hosts, h)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
	return nil
}

// drainHost stops new placements on a host and moves its live Runs
// elsewhere (stop → auto-resume).
func (s *Server) drainHost(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	id := r.PathValue("id")
	err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE hosts SET draining = true, state = CASE WHEN state = 'ready' THEN 'draining' ELSE state END,
				drain_requested_at = coalesce(drain_requested_at, now())
			WHERE (id = $1 OR name = $1) AND tenant_id = $2 AND state <> 'terminated'`, id, p.TenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		return s.drainPlacements(r.Context(), tx, id)
	})
	if err != nil {
		return err
	}
	s.hub.Notify(id)
	writeJSON(w, http.StatusAccepted, map[string]any{"draining": true})
	return nil
}

func (s *Server) drainPlacements(ctx context.Context, tx pgx.Tx, host string) error {
	rows, err := tx.Query(ctx, `SELECT p.run_id, p.tenant_id FROM placements p JOIN hosts h ON h.id = p.host_id
		WHERE (h.id = $1 OR h.name = $1) AND p.state IN ('assigned', 'starting', 'running', 'stopping')`, host)
	if err != nil {
		return err
	}
	type pr struct{ run, tenant string }
	var live []pr
	for rows.Next() {
		var x pr
		if err := rows.Scan(&x.run, &x.tenant); err != nil {
			rows.Close()
			return err
		}
		live = append(live, x)
	}
	rows.Close()
	for _, x := range live {
		if _, err := s.requestStop(ctx, tx, x.tenant, x.run, "drain"); err != nil {
			return err
		}
	}
	return nil
}

type Pool struct {
	Name      string         `json:"name"`
	Provider  string         `json:"provider"`
	Template  map[string]any `json:"template,omitempty"`
	MinHosts  int            `json:"minHosts"`
	MaxHosts  int            `json:"maxHosts"`
	WarmHosts int            `json:"warmHosts"`
	Shared    bool           `json:"shared"`
	Platform  bool           `json:"platform"`
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	pools := []Pool{}
	err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT name, provider, template, min_hosts, max_hosts, warm_hosts, shared, tenant_id IS NULL
			FROM pools WHERE tenant_id = $1 OR tenant_id IS NULL ORDER BY name`, p.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pl Pool
			if err := rows.Scan(&pl.Name, &pl.Provider, &pl.Template, &pl.MinHosts, &pl.MaxHosts, &pl.WarmHosts, &pl.Shared, &pl.Platform); err != nil {
				return err
			}
			pools = append(pools, pl)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
	return nil
}

// putPool creates or updates one of the tenant's pools.
func (s *Server) putPool(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	var pl Pool
	if err := readJSON(r, &pl); err != nil {
		return err
	}
	if pl.Name == "" || (pl.Provider != "static" && pl.Provider != "ec2") {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "name and provider (static | ec2) are required")
	}
	if pl.Shared {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "only platform pools can be shared (luxd admin create-pool --shared)")
	}
	if pl.Template == nil {
		pl.Template = map[string]any{}
	}
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `INSERT INTO pools (id, tenant_id, name, provider, template, min_hosts, max_hosts, warm_hosts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET provider = EXCLUDED.provider, template = EXCLUDED.template,
				min_hosts = EXCLUDED.min_hosts, max_hosts = EXCLUDED.max_hosts, warm_hosts = EXCLUDED.warm_hosts`,
			ids.New(ids.Pool), p.TenantID, pl.Name, pl.Provider, pl.Template, pl.MinHosts, pl.MaxHosts, pl.WarmHosts)
		return err
	})
	if err != nil {
		return err
	}
	s.Kick()
	writeJSON(w, http.StatusOK, pl)
	return nil
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
