package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// POST /v1/runs/{id}/terminate and its deprecated alias /cancel both end a
// Run terminated, through the HTTP routes: a live one is asked to stop
// (a terminate message, reason terminate) and ends terminated once its
// placement exits; a stopped one ends terminated at once. Either way its
// held secrets go, and the request is recorded as terminate.requested.
func TestTerminateRoutes(t *testing.T) {
	for _, path := range []string{"terminate", "cancel"} {
		t.Run(path+"/live", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			key := apiKey(t, s, new("t1"), "run")
			if code, body := call(t, s, key, http.MethodPost, "/v1/runs/r1/"+path, nil); code != http.StatusAccepted {
				t.Fatalf("%s: %d %s", path, code, body)
			}
			var msgType, stopReason, runState, runReason string
			var requested bool
			systemScan(t, s, `SELECT (SELECT type FROM host_messages WHERE run_id = 'r1' AND type IN ($1, $2)),
					p.stop_reason, r.state, r.state_reason, r.terminate_requested
				FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
				[]any{proto.MsgStop, proto.MsgTerminate}, &msgType, &stopReason, &runState, &runReason, &requested)
			if msgType != proto.MsgTerminate || stopReason != "terminate" || runState != StateStopping || runReason != "terminate" || !requested {
				t.Fatalf("after %s: message %q stop %q state %q %q requested %v", path, msgType, stopReason, runState, runReason, requested)
			}
			exitR1(t, s)
			assertTerminated(t, s, "terminated")
		})
		t.Run(path+"/stopped", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			exitR1(t, s)
			s.secrets.put("r1", map[string]string{"TOKEN": "jit"})
			key := apiKey(t, s, new("t1"), "run")
			if code, body := call(t, s, key, http.MethodPost, "/v1/runs/r1/"+path, nil); code != http.StatusAccepted {
				t.Fatalf("%s: %d %s", path, code, body)
			}
			assertTerminated(t, s, "terminated")
			// Idempotent: a second terminate changes nothing.
			var before string
			systemScan(t, s, `SELECT updated_at::text FROM runs WHERE id = 'r1'`, nil, &before)
			if code, _ := call(t, s, key, http.MethodPost, "/v1/runs/r1/"+path, nil); code != http.StatusAccepted {
				t.Fatalf("second %s: %d", path, code)
			}
			var after string
			systemScan(t, s, `SELECT updated_at::text FROM runs WHERE id = 'r1'`, nil, &after)
			if after != before {
				t.Fatal("a second terminate wrote the Run")
			}
		})
	}
}

// assertTerminated checks r1 ended terminated with reason, its secrets
// dropped and one terminate.requested event.
func assertTerminated(t *testing.T, s *Server, reason string) {
	t.Helper()
	var state, got string
	var requests int
	systemScan(t, s, `SELECT state, state_reason, (SELECT count(*) FROM run_events WHERE run_id = 'r1' AND type = 'terminate.requested')
		FROM runs WHERE id = 'r1'`, nil, &state, &got, &requests)
	if _, held := s.secrets.get("r1"); state != StateTerminated || got != reason || requests != 1 || held {
		t.Fatalf("state %q reason %q terminate.requested %d secrets held %v; want terminated %q 1 false", state, got, requests, held, reason)
	}
}

// A placement whose stop reason an older luxd sharing the database wrote
// as cancel still ends its Run terminated.
func TestLegacyCancelStopReasonTerminates(t *testing.T) {
	s, ctx := policyFixture(t, "")
	execSQL(t, s, ctx, `UPDATE placements SET stop_reason = 'cancel', stop_requested_at = now(), state = 'stopping' WHERE id = 'p1'`)
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)
	exitR1(t, s)
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "terminated" {
		t.Fatalf("state %q reason %q, want terminated", state, reason)
	}
}
