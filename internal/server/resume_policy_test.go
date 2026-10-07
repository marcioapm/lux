package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// policyFixture is a running Run r1 on h1 (h2 ready too) whose spec has
// resumePolicy policy ("" for none), with a server and secret values in
// luxd's cache.
func policyFixture(t *testing.T, policy string) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	namedPools(t, s, "default")
	ctx := context.Background()
	sp := `{"placement": {"pool": "default"}}`
	if policy != "" {
		sp = `{"placement": {"pool": "default"}, "resumePolicy": "` + policy + `"}`
	}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	readyHost(t, s, "h1", "default", "", false)
	readyHost(t, s, "h2", "default", "", false)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', $1, 'running', 1)`, sp)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)
	execSQL(t, s, ctx, `INSERT INTO run_servers (id, tenant_id, run_id, name, port, command, state, epoch)
		VALUES ('srv_webwebwebwebwebw', 't1', 'r1', 'web', 3000, '["serve"]', 'ready', 1)`)
	s.secrets.put("r1", map[string]string{"TOKEN": "jit"})
	return s, ctx
}

func operatorCtx() context.Context {
	return context.WithValue(context.Background(), principalKey, Principal{Operator: true, TenantID: "t1", Scopes: []string{"admin", "operator"}})
}

// moveStops stops r1 the way each move does.
var moveStops = map[string]func(t *testing.T, s *Server) error{
	"preempt": func(t *testing.T, s *Server) error {
		return s.hostEvicting(context.Background(), "h1", proto.Evicting{Reason: "spot"})
	},
	"drain": func(t *testing.T, s *Server) error {
		_, err := s.drainHost(operatorCtx(), &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}})
		return err
	},
	"migrate": func(t *testing.T, s *Server) error {
		_, err := s.migrateRun(operatorCtx(), &migrateRunInput{RunPath: RunPath{ID: "r1"}})
		return err
	},
}

// r1Moving is whether r1's server reads as moving.
func r1Moving(t *testing.T, s *Server) bool {
	t.Helper()
	var rows []serverRow
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		var err error
		rows, err = collectServerRows(tx.Query(context.Background(), serverSelect+`WHERE sv.run_id = 'r1'`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d servers", len(rows))
	}
	return rows[0].Moving
}

// exitR1 reports r1's placement exited as a stopped workload's does.
func exitR1(t *testing.T, s *Server) {
	t.Helper()
	code := 143
	f := proto.Frame{Type: proto.MsgStatus, ID: 1, RunID: "r1", Epoch: 1,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})}
	if got := s.handleReport(context.Background(), "h1", f); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s %s", got.Type, got.Data)
	}
}

// With resumePolicy never, a move that stops the Run ends it failed, its
// reason naming the move, with no new placement, its secrets dropped and
// its servers stopped as for any end; with auto or unset it is resumed.
func TestResumePolicyOnMove(t *testing.T) {
	for _, stop := range []string{"preempt", "drain", "migrate"} {
		for _, policy := range []string{"never", "auto", ""} {
			t.Run(stop+"/"+policy, func(t *testing.T) {
				never := policy == "never"
				s, ctx := policyFixture(t, policy)
				err := moveStops[stop](t, s)
				if stop == "migrate" && never {
					var he *HTTPError
					if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_movable" {
						t.Fatalf("migrate: %v, want 409 not_movable", err)
					}
					var state string
					var stopRequested bool
					systemScan(t, s, `SELECT r.state, p.stop_requested_at IS NOT NULL FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
						nil, &state, &stopRequested)
					if state != StateRunning || stopRequested {
						t.Fatalf("after a refused migrate: state %q stop requested %v, want running and none", state, stopRequested)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := r1Moving(t, s); got == never {
					t.Errorf("stopping for %s: moving %v", stop, got)
				}
				exitR1(t, s)
				var svStop string
				systemScan(t, s, `SELECT coalesce(stop_reason, '') FROM run_servers WHERE run_id = 'r1'`, nil, &svStop)
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}

				var state, reason string
				var placements int
				systemScan(t, s, `SELECT r.state, r.state_reason, (SELECT count(*) FROM placements WHERE run_id = r.id)
					FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &placements)
				_, cached := s.secrets.get("r1")
				if never {
					want := stop + ": not resumed (resumePolicy never)"
					if state != StateFailed || reason != want || placements != 1 || cached || svStop != "run stopped" {
						t.Errorf("state %q reason %q placements %d secrets cached %v server %q; want failed %q 1 false \"run stopped\"",
							state, reason, placements, cached, svStop, want)
					}
					return
				}
				if state == StateFailed || state == StateStopped || placements != 2 || !cached || svStop != "migrated" {
					t.Errorf("state %q reason %q placements %d secrets cached %v server %q; want resumed and placed again, secrets kept, server migrated",
						state, reason, placements, cached, svStop)
				}
			})
		}
	}
}

// A cordon-only drain stops nothing, whatever the policy: the Run finishes
// where it is.
func TestResumePolicyNeverCordonOnlyDrain(t *testing.T) {
	s, _ := policyFixture(t, "never")
	if _, err := s.drainHost(operatorCtx(), &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
		t.Fatal(err)
	}
	var state string
	var stopRequested bool
	systemScan(t, s, `SELECT r.state, p.stop_requested_at IS NOT NULL FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
		nil, &state, &stopRequested)
	if state != StateRunning || stopRequested {
		t.Fatalf("state %q stop requested %v, want running and none", state, stopRequested)
	}
}

// A resume a person asks for after a never Run failed is not refused.
func TestResumePolicyNeverResumableByHand(t *testing.T) {
	s, _ := policyFixture(t, "never")
	if err := moveStops["preempt"](t, s); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	// The fixture's Run declares no secrets, so the resume needs none.
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
	if state != StateResuming {
		t.Fatalf("after resume: %q", state)
	}
}
