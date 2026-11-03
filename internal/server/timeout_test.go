package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
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

func dur(s string) *spec.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return &spec.Duration{Duration: d}
}

// changeR1Timeout posts body to r1's /timeout as tenant t1.
func changeR1Timeout(s *Server, body TimeoutRequest) (*runOutput, error) {
	return s.timeoutRun(tenantCtx("t1"), &timeoutRunInput{RunPath: RunPath{ID: "r1"}, Body: body})
}

// r1Timeout is r1's stored spec.timeout ("" for none).
func r1Timeout(t *testing.T, s *Server) string {
	t.Helper()
	return queryOne[string](t, s, `SELECT coalesce(spec->>'timeout', '') FROM runs WHERE id = 'r1'`)
}

// wantHTTPError fails unless err is an HTTPError with status and code.
func wantHTTPError(t *testing.T, err error, status int, code string) *HTTPError {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != status || he.Code != code {
		t.Fatalf("err %v, want %d %s", err, status, code)
	}
	return he
}

// timeoutEvents is r1's timeout.changed events' from→to, in order.
func timeoutEvents(t *testing.T, s *Server) []string {
	t.Helper()
	var got []string
	systemScan(t, s, `SELECT coalesce(array_agg((data->>'from') || '→' || (data->>'to') || ' by ' || (data->>'by') ORDER BY id), '{}')
		FROM run_events WHERE run_id = 'r1' AND type = 'timeout.changed'`, nil, &got)
	return got
}

func TestTimeoutChange(t *testing.T) {
	s, _ := timeoutFixture(t, "30 minutes")
	out, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")})
	if err != nil {
		t.Fatal(err)
	}
	if got := r1Timeout(t, s); got != "2h0m0s" || out.Body.Spec.Timeout.Duration != 2*time.Hour {
		t.Fatalf("after add: stored %q, answered %v", got, out.Body.Spec.Timeout)
	}
	if _, err := changeR1Timeout(s, TimeoutRequest{Set: dur("4h")}); err != nil {
		t.Fatal(err)
	}
	// The same value again: no change, no event.
	if _, err := changeR1Timeout(s, TimeoutRequest{Set: dur("4h")}); err != nil {
		t.Fatal(err)
	}
	if _, err := changeR1Timeout(s, TimeoutRequest{Unlimited: true}); err != nil {
		t.Fatal(err)
	}
	if got := r1Timeout(t, s); got != "" {
		t.Fatalf("after unlimited: %q", got)
	}
	// Nothing to add to.
	wantHTTPError(t, func() error { _, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")}); return err }(), 422, "invalid_spec")
	want := []string{"1h0m0s→2h0m0s by k-t1", "2h0m0s→4h0m0s by k-t1", "4h0m0s→ by k-t1"}
	if got := timeoutEvents(t, s); !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}

func TestTimeoutChangeBodies(t *testing.T) {
	s, _ := timeoutFixture(t, "1 minute")
	for name, body := range map[string]TimeoutRequest{
		"empty":         {},
		"two":           {Add: dur("1h"), Set: dur("2h")},
		"set and none":  {Set: dur("2h"), Unlimited: true},
		"zero add":      {Add: dur("0s")},
		"negative set":  {Set: dur("-1h")},
		"unknown field": {decodeErr: errors.New(`unknown field "extend"`)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := changeR1Timeout(s, body)
			wantHTTPError(t, err, 422, "invalid_spec")
		})
	}
	if got := r1Timeout(t, s); got != "1h" {
		t.Fatalf("timeout %q after refused bodies", got)
	}
}

func TestTimeoutChangeTooShort(t *testing.T) {
	s, _ := timeoutFixture(t, "50 minutes")
	he := wantHTTPError(t, func() error { _, err := changeR1Timeout(s, TimeoutRequest{Set: dur("50m")}); return err }(), 422, "timeout_too_short")
	if !strings.Contains(he.Message, "has run 50m") || !strings.Contains(he.Message, "least it may be given is 51m") {
		t.Fatalf("message %q", he.Message)
	}
	if got := r1Timeout(t, s); got != "1h" {
		t.Fatalf("timeout %q after a refused change", got)
	}
	if _, err := changeR1Timeout(s, TimeoutRequest{Set: dur("52m")}); err != nil {
		t.Fatalf("52m after 50m run: %v", err)
	}
}

func TestTimeoutChangeRefusedStates(t *testing.T) {
	t.Run("terminated", func(t *testing.T) {
		s, ctx := timeoutFixture(t, "1 minute")
		execSQL(t, s, ctx, `UPDATE runs SET state = 'terminated' WHERE id = 'r1'`)
		_, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")})
		wantHTTPError(t, err, 409, "run_terminated")
	})
	t.Run("timeout stop started", func(t *testing.T) {
		s, ctx := timeoutFixture(t, "2 hours")
		if err := s.reapTimeouts(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := changeR1Timeout(s, TimeoutRequest{Add: dur("4h")})
		wantHTTPError(t, err, 409, "timeout_stop_started")
		if got := r1Timeout(t, s); got != "1h" {
			t.Fatalf("timeout %q", got)
		}
	})
	t.Run("other stop started", func(t *testing.T) {
		s, _ := timeoutFixture(t, "1 minute")
		if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")}); err != nil {
			t.Fatalf("while stopping for a stop: %v", err)
		}
	})
	t.Run("failed on its timeout", func(t *testing.T) {
		s, ctx := timeoutFixture(t, "2 hours")
		failOnTimeout(t, s, ctx)
		if _, err := changeR1Timeout(s, TimeoutRequest{Set: dur("3h")}); err != nil {
			t.Fatalf("failed Run: %v", err)
		}
		if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
			t.Fatal(err)
		}
		startEpoch2(t, s, ctx)
		if err := s.reapTimeouts(ctx); err != nil {
			t.Fatal(err)
		}
		if state, reason := r1Stop(t, s); state != StateRunning || reason != "" {
			t.Fatalf("resumed after extend: %s %q", state, reason)
		}
	})
}

// Two adds at once both count: each reads the other's committed timeout.
func TestTimeoutConcurrentAdds(t *testing.T) {
	s, _ := timeoutFixture(t, "1 minute")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := r1Timeout(t, s); got != "9h0m0s" {
		t.Fatalf("timeout %q after 8 concurrent adds to 1h", got)
	}
	if n := len(timeoutEvents(t, s)); n != 8 {
		t.Fatalf("%d events", n)
	}
}

// The reaper holds a running Run to its changed timeout, in its current placement.
func TestTimeoutReaperFollowsChange(t *testing.T) {
	s, ctx := timeoutFixture(t, "50 minutes")
	if _, err := changeR1Timeout(s, TimeoutRequest{Add: dur("1h")}); err != nil {
		t.Fatal(err)
	}
	// Past the old timeout, within the new one.
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '90 minutes' WHERE id = 'p1'`)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := r1Stop(t, s); state != StateRunning || reason != "" {
		t.Fatalf("within the new timeout: %s %q", state, reason)
	}
	// Past the new one.
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '121 minutes' WHERE id = 'p1'`)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := r1Stop(t, s); state != StateStopping || reason != "timeout" {
		t.Fatalf("past the new timeout: %s %q", state, reason)
	}
}

// timeoutRemainingSeconds, on GET and list, counts runtime as the reaper
// does: the Run is stopped exactly when it reaches 0.
func TestTimeoutRemainingAgreesWithReaper(t *testing.T) {
	s, ctx := timeoutFixture(t, "50 minutes")
	remaining := func() *float64 {
		t.Helper()
		got, err := s.getRun(tenantCtx("t1"), &RunPath{ID: "r1"})
		if err != nil {
			t.Fatal(err)
		}
		list, err := s.listRuns(tenantCtx("t1"), &listRunsInput{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Body.Runs) != 1 || (list.Body.Runs[0].TimeoutRemainingSeconds == nil) != (got.Body.TimeoutRemainingSeconds == nil) {
			t.Fatalf("list %+v", list.Body.Runs)
		}
		if got.Body.TimeoutRemainingSeconds != nil && got.Body.Timeout != got.Body.Spec.Timeout.String() {
			t.Fatalf("timeout %q, spec %s", got.Body.Timeout, got.Body.Spec.Timeout)
		}
		return got.Body.TimeoutRemainingSeconds
	}
	if r := remaining(); r == nil || *r < 598 || *r > 601 {
		t.Fatalf("remaining %v, want 600", r)
	}
	// An earlier placement that ended without an end time: the reaper
	// counts it to now, and so does the remaining time.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at) VALUES ('p0', 't1', 'r1', 'h1', 0, 'exited', now() - interval '5 minutes')`)
	if r := remaining(); r == nil || *r < 298 || *r > 301 {
		t.Fatalf("remaining %v, want 300", r)
	}
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ := r1Stop(t, s); state != StateRunning {
		t.Fatalf("stopped with time left: %s", state)
	}
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '56 minutes' WHERE id = 'p1'`)
	if r := remaining(); r == nil || *r != 0 {
		t.Fatalf("remaining %v, want 0", r)
	}
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := r1Stop(t, s); state != StateStopping || reason != "timeout" {
		t.Fatalf("at 0 left: %s %q", state, reason)
	}
	execSQL(t, s, ctx, `UPDATE runs SET spec = spec - 'timeout' WHERE id = 'r1'`)
	if r := remaining(); r != nil {
		t.Fatalf("remaining %v without a timeout", *r)
	}
}

func resumeR1(s *Server, req resumeRequest) (*resumeOutput, error) {
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}, Body: &req})
}

// A Run failed on its timeout, resumed with a longer one, runs on; resumed
// without one it is stopped again at once (TestTimeoutFailedRunResumesAndTimesOutAgain).
func TestTimeoutResumeWithTimeout(t *testing.T) {
	s, ctx := timeoutFixture(t, "2 hours")
	failOnTimeout(t, s, ctx)
	_, err := resumeR1(s, resumeRequest{Timeout: dur("2h")})
	he := wantHTTPError(t, err, 422, "timeout_too_short")
	if !strings.Contains(he.Message, "has run 2h0m0s") {
		t.Fatalf("message %q", he.Message)
	}
	wantHTTPError(t, func() error { _, err := resumeR1(s, resumeRequest{Timeout: dur("-1h")}); return err }(), 422, "invalid_spec")
	wantHTTPError(t, func() error { _, err := resumeR1(s, resumeRequest{Timeout: dur("2h"), ValidateOnly: true}); return err }(), 422, "timeout_too_short")
	if out, err := resumeR1(s, resumeRequest{Timeout: dur("3h"), ValidateOnly: true}); err != nil || !out.Body.Valid {
		t.Fatalf("validateOnly: %+v %v", out, err)
	}
	if got := r1Timeout(t, s); got != "1h" || len(timeoutEvents(t, s)) != 0 {
		t.Fatalf("after refused and dry resumes: timeout %q events %q", got, timeoutEvents(t, s))
	}
	if _, err := resumeR1(s, resumeRequest{Timeout: dur("3h")}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := r1Timeout(t, s); got != "3h0m0s" {
		t.Fatalf("timeout %q", got)
	}
	// Already resuming: the same timeout is the same resume, another is refused.
	if _, err := resumeR1(s, resumeRequest{Timeout: dur("3h")}); err != nil {
		t.Fatalf("same timeout again: %v", err)
	}
	wantHTTPError(t, func() error { _, err := resumeR1(s, resumeRequest{Timeout: dur("4h")}); return err }(), 409, "not_resumable")
	startEpoch2(t, s, ctx)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := r1Stop(t, s); state != StateRunning || reason != "" {
		t.Fatalf("resumed with 3h: %s %q", state, reason)
	}
	if got, want := timeoutEvents(t, s), []string{"1h0m0s→3h0m0s by k-t1"}; !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// "0s" on resume removes the timeout.
func TestTimeoutResumeUnlimited(t *testing.T) {
	s, ctx := timeoutFixture(t, "2 hours")
	failOnTimeout(t, s, ctx)
	if _, err := resumeR1(s, resumeRequest{Timeout: dur("0s")}); err != nil {
		t.Fatal(err)
	}
	if got := r1Timeout(t, s); got != "" {
		t.Fatalf("timeout %q", got)
	}
	startEpoch2(t, s, ctx)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ := r1Stop(t, s); state != StateRunning {
		t.Fatalf("resumed unlimited: %s", state)
	}
}
