package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
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
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyAdapterEvent(ctx, tx, "t1", "r1", 1, proto.AdapterEvent{Activity: activity})
	}); err != nil {
		t.Fatal(err)
	}
}

func assertIdleReap(t *testing.T, s *Server, ctx context.Context, stopped bool) {
	t.Helper()
	if err := s.reapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	_, got, reason := drainState(t, s, ctx, "h1", "r1")
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
	} {
		t.Run(c.name, func(t *testing.T) { s, ctx := idleFixture(t); c.setup(t, s, ctx); assertIdleReap(t, s, ctx, c.stop) })
	}
}

// A bare interrupt never reports an input phase; one carrying text does.
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

func TestIdleReaperPendingInput(t *testing.T) {
	for _, phase := range []string{"undelivered", "delivered", "consumed", "failed", "no receipt", "no receipt busy", "new input", "new consumed input"} {
		t.Run(phase, func(t *testing.T) {
			s, ctx := idleFixture(t)
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return addEvent(ctx, tx, "t1", "r1", 1, "input", map[string]any{"requestId": "req-1"})
			}); err != nil {
				t.Fatal(err)
			}
			if phase != "undelivered" && phase != "new input" {
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					ev := proto.AdapterEvent{InputAck: "req-1", InputPhase: proto.InputAccepted, InputLands: "next_step", InputReceipt: phase != "no receipt" && phase != "no receipt busy"}
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
			if phase != "new input" && phase != "new consumed input" {
				ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
			}
			assertIdleReap(t, s, ctx, phase == "consumed" || phase == "failed" || phase == "no receipt busy")
		})
	}
}
