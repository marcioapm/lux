package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func idleFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload":{"adapter":"acp","idleTimeout":"1m"}}', first_started_at = now() - interval '1 hour', activity = 'idle' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '1 hour' WHERE id = 'p1'`)
	idleActivity(t, s, ctx, "idle")
	ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
	return s, ctx
}

func idleActivity(t *testing.T, s *Server, ctx context.Context, activity string) {
	t.Helper()
	idleEvent(t, s, ctx, 1, proto.AdapterEvent{Activity: activity})
}

func idleEvent(t *testing.T, s *Server, ctx context.Context, epoch int, ev proto.AdapterEvent) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyAdapterEvent(ctx, tx, "t1", "r1", epoch, ev)
	}); err != nil {
		t.Fatal(err)
	}
}

// assertIdleReap runs the reaper and checks the stop of r1's current placement.
func assertIdleReap(t *testing.T, s *Server, ctx context.Context, stopped bool) {
	t.Helper()
	if err := s.reapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	var got bool
	var reason string
	systemScan(t, s, `SELECT p.stop_requested_at IS NOT NULL, p.stop_reason FROM placements p
		JOIN runs r ON r.id = p.run_id AND p.epoch = r.current_epoch WHERE r.id = 'r1'`, nil, &got, &reason)
	if got != stopped || (stopped && reason != "idle") {
		t.Fatalf("stop=%v reason=%q, want stop=%v idle", got, reason, stopped)
	}
}

func TestIdleReaperStopsOnceAndEndsStopped(t *testing.T) {
	s, ctx := idleFixture(t)
	assertIdleReap(t, s, ctx, true)
	assertIdleReap(t, s, ctx, true)
	var n int
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE run_id = 'r1' AND type = 'stop'`, nil, &n)
	if n != 1 {
		t.Fatalf("stop messages=%d", n)
	}
	var seconds float64
	systemScan(t, s, `SELECT (data->>'idleSeconds')::float8 FROM run_events WHERE run_id = 'r1' AND type = 'idle'`, nil, &seconds)
	if seconds < 120 {
		t.Fatalf("idle seconds=%v", seconds)
	}
	code := 0
	f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, ID: 1, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code})})
	if f.Type != proto.MsgAck {
		t.Fatalf("exit: %s %s", f.Type, f.Data)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateStopped || reason != "idle" || !resumable(state) {
		t.Fatalf("state=%s reason=%s", state, reason)
	}
	var stateEvent string
	systemScan(t, s, `SELECT data->>'reason' FROM run_events WHERE run_id = 'r1' AND type = 'state' AND data->>'state' = 'stopped'`, nil, &stateEvent)
	if stateEvent != "idle" {
		t.Fatalf("stopped state event reason=%q", stateEvent)
	}
}

func TestIdleTimeoutGenericSubmission(t *testing.T) {
	s, _ := costFixture(t)
	key := apiKey(t, s, new("t1"), "run", "read")
	response := submitHTTP(t, s, key, "idle-generic", json.RawMessage(`{"image":{"ref":"alpine"},"workload":{"adapter":"generic","command":["true"],"idleTimeout":"1m"}}`))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_spec") || !strings.Contains(response.Body.String(), "workload.idleTimeout") {
		t.Fatalf("submission: %d %s", response.Code, response.Body)
	}
}

func TestRuntimeTimeoutStillFails(t *testing.T) {
	s, ctx := idleFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"timeout":"1m"}' WHERE id = 'r1'`)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	code := 0
	f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, ID: 1, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code})})
	if f.Type != proto.MsgAck {
		t.Fatalf("exit: %s %s", f.Type, f.Data)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateFailed || reason != "timeout" {
		t.Fatalf("state=%s reason=%s", state, reason)
	}
}

func TestIdleReaperClockAndGuards(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*testing.T, *Server, context.Context)
		stop  bool
	}{
		{"due", func(t *testing.T, s *Server, ctx context.Context) {}, true},
		{"repeated idle", func(t *testing.T, s *Server, ctx context.Context) { idleActivity(t, s, ctx, "idle") }, true},
		{"busy", func(t *testing.T, s *Server, ctx context.Context) { idleActivity(t, s, ctx, "busy") }, false},
		{"busy resets", func(t *testing.T, s *Server, ctx context.Context) {
			idleActivity(t, s, ctx, "busy")
			idleActivity(t, s, ctx, "idle")
		}, false},
		{"no timeout", func(t *testing.T, s *Server, ctx context.Context) {
			execSQL(t, s, ctx, `UPDATE runs SET spec = '{}' WHERE id = 'r1'`)
		}, false},
		{"zero", func(t *testing.T, s *Server, ctx context.Context) {
			execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload":{"idleTimeout":"0s"}}' WHERE id = 'r1'`)
		}, false},
		{"fresh placement", func(t *testing.T, s *Server, ctx context.Context) {
			execSQL(t, s, ctx, `UPDATE placements SET started_at = now() WHERE id = 'p1'`)
		}, false},
		{"already stopping", func(t *testing.T, s *Server, ctx context.Context) {
			execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)
		}, false},
		{"first prompt delivered", func(t *testing.T, s *Server, ctx context.Context) {
			// The first prompt has no input event; its receipt alone keeps it pending.
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return s.applyAdapterEvent(ctx, tx, "t1", "r1", 1, proto.AdapterEvent{InputAck: "prompt", InputPhase: proto.InputAccepted, InputLands: "next_step", InputReceipt: true})
			}); err != nil {
				t.Fatal(err)
			}
			ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) { s, ctx := idleFixture(t); c.setup(t, s, ctx); assertIdleReap(t, s, ctx, c.stop) })
	}
}

// A stop requested before runs.state moves to stopping is not stopped again.
func TestIdleReaperSkipsRequestedStop(t *testing.T) {
	s, ctx := idleFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET stop_requested_at = now(), stop_reason = 'stop' WHERE id = 'p1'`)
	if err := s.reapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	var stops int
	systemScan(t, s, `SELECT r.state, p.stop_reason, (SELECT count(*) FROM host_messages WHERE run_id = 'r1' AND type = 'stop')
		FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`, nil, &state, &reason, &stops)
	if state != StateRunning || reason != "stop" || stops != 0 {
		t.Fatalf("state=%s reason=%s stop messages=%d", state, reason, stops)
	}
}

// A bare interrupt never reports an input phase; one carrying content does.
func TestIdleReaperInterruptInput(t *testing.T) {
	for _, c := range []struct {
		name string
		body inputRequest
		aged bool
		stop bool
	}{
		{"interrupt only", inputRequest{Interrupt: true}, true, true},
		{"interrupt only moves clock", inputRequest{Interrupt: true}, false, false},
		{"interrupt with text", inputRequest{Interrupt: true, Text: "stop and look"}, true, false},
		{"interrupt with attachments", inputRequest{Interrupt: true, Attachments: []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: b64(tinyPNG)}}}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := idleFixture(t)
			actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"run"}})
			if _, err := s.postInput(actx, &postInputInput{RunPath: RunPath{ID: "r1"}, Body: c.body}); err != nil {
				t.Fatal(err)
			}
			if c.aged {
				ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
			}
			assertIdleReap(t, s, ctx, c.stop)
		})
	}
}

// A resumed placement (epoch 2) is judged on its own events only.
func TestIdleReaperResumedEpoch(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*testing.T, *Server, context.Context)
		stop  bool
	}{
		{"old undelivered input", func(t *testing.T, s *Server, ctx context.Context) {
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return addEvent(ctx, tx, "t1", "r1", 1, "input", map[string]any{"requestId": "req-old"})
			}); err != nil {
				t.Fatal(err)
			}
			ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
		}, true},
		{"old input", func(t *testing.T, s *Server, ctx context.Context) {
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return addEvent(ctx, tx, "t1", "r1", 1, "input", map[string]any{"requestId": "req-old", "interrupt": true, "text": "", "rawBytes": 0})
			}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"old turn", func(t *testing.T, s *Server, ctx context.Context) {
			idleEvent(t, s, ctx, 1, proto.AdapterEvent{Activity: "busy"})
			idleEvent(t, s, ctx, 1, proto.AdapterEvent{Activity: "idle"})
		}, true},
		{"new turn", func(t *testing.T, s *Server, ctx context.Context) {
			idleEvent(t, s, ctx, 2, proto.AdapterEvent{Activity: "busy"})
			idleEvent(t, s, ctx, 2, proto.AdapterEvent{Activity: "idle"})
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := idleFixture(t)
			execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'p1'`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at) VALUES ('p2', 't1', 'r1', 'h1', 2, 'running', now() - interval '1 hour')`)
			execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'r1'`)
			idleEvent(t, s, ctx, 2, proto.AdapterEvent{Activity: "idle"})
			ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
			c.setup(t, s, ctx)
			assertIdleReap(t, s, ctx, c.stop)
		})
	}
}

func TestIdleReaperPendingInput(t *testing.T) {
	for _, phase := range []string{"undelivered", "undelivered turn", "delivered", "consumed", "failed", "no receipt", "no receipt busy", "no receipt steer", "no receipt short turn", "new input", "new consumed input"} {
		t.Run(phase, func(t *testing.T) {
			s, ctx := idleFixture(t)
			if phase == "no receipt steer" {
				// The steer joins a running turn: busy precedes the input and no busy follows it.
				idleActivity(t, s, ctx, "busy")
			}
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return addEvent(ctx, tx, "t1", "r1", 1, "input", map[string]any{"requestId": "req-1"})
			}); err != nil {
				t.Fatal(err)
			}
			if phase == "no receipt short turn" || phase == "undelivered turn" {
				// The turn ends before its receipt-free delivery is recorded.
				idleActivity(t, s, ctx, "busy")
				idleActivity(t, s, ctx, "idle")
			}
			if phase != "undelivered" && phase != "undelivered turn" && phase != "new input" {
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					ev := proto.AdapterEvent{InputAck: "req-1", InputPhase: proto.InputAccepted, InputLands: "next_step", InputReceipt: !strings.HasPrefix(phase, "no receipt")}
					if err := s.applyAdapterEvent(ctx, tx, "t1", "r1", 1, ev); err != nil {
						return err
					}
					if phase == "consumed" || phase == "failed" || phase == "new consumed input" {
						progressPhase := phase
						if phase == "new consumed input" {
							progressPhase = "consumed"
						}
						return applyInputProgress(ctx, tx, "t1", "r1", 1, proto.InputProgress{RequestID: "req-1", Phase: progressPhase})
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "no receipt busy" {
				idleActivity(t, s, ctx, "busy")
				idleActivity(t, s, ctx, "idle")
			}
			if phase == "no receipt steer" {
				idleActivity(t, s, ctx, "idle")
			}
			if phase != "new input" && phase != "new consumed input" {
				ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
			}
			assertIdleReap(t, s, ctx, phase == "consumed" || phase == "failed" || phase == "no receipt busy" || phase == "no receipt steer" || phase == "no receipt short turn")
		})
	}
}
