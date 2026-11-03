package server

import (
	"context"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// timeoutFixture is policyFixture's running r1 with a 1h timeout whose
// placement has run for runFor (a Postgres interval).
func timeoutFixture(t *testing.T, runFor string) (*Server, context.Context) {
	t.Helper()
	s, ctx := policyFixture(t, "")
	execSQL(t, s, ctx, `UPDATE runs SET spec = jsonb_set(spec, '{timeout}', '"1h"'), first_started_at = now() - $1::interval WHERE id = 'r1'`, runFor)
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - $1::interval WHERE id = 'p1'`, runFor)
	return s, ctx
}

// r1Stop is r1's state and its current placement's stop reason.
func r1Stop(t *testing.T, s *Server) (state, stopReason string) {
	t.Helper()
	systemScan(t, s, `SELECT r.state, p.stop_reason FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch WHERE r.id = 'r1'`,
		nil, &state, &stopReason)
	return state, stopReason
}

// failOnTimeout runs r1 into its timeout and reports it exited: failed, timeout.
func failOnTimeout(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateFailed || reason != "timeout" {
		t.Fatalf("after timeout: %s %s, want failed timeout", state, reason)
	}
}

// startEpoch2 schedules a resumed r1 and reports its new placement running.
func startEpoch2(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id = 'r1' AND epoch = 2`); n != 1 {
		t.Fatalf("epoch 2 placements: %d", n)
	}
	applyRunStatus(t, s, "r1", 2, proto.Status{State: "running"})
}

// A Run failed on its timeout resumes, and with its time used up is
// stopped for its timeout again on the next reaper tick.
func TestTimeoutFailedRunResumesAndTimesOutAgain(t *testing.T) {
	s, ctx := timeoutFixture(t, "2 hours")
	failOnTimeout(t, s, ctx)
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	startEpoch2(t, s, ctx)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := r1Stop(t, s); state != StateStopping || reason != "timeout" {
		t.Fatalf("resumed: %s %q, want stopping timeout", state, reason)
	}
}
