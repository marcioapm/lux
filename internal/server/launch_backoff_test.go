package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// backoffFixture: planningFixture with one waiting Run, a provider whose
// launches fail with a non-capacity error, and a clock the test moves.
func backoffFixture(t *testing.T) (*Server, poolRow, *planningProvider, *time.Time) {
	t.Helper()
	s, pl, p := planningFixture(t, 1)
	p.fail = true
	now := time.Date(2026, 10, 7, 13, 52, 16, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, pl, p, &now
}

// backoffPass is one provisioner pass for the pool; a launch error is
// expected on an attempt and ignored.
func backoffPass(s *Server, pl poolRow, p *planningProvider) bool {
	before := p.calls
	_ = s.reconcilePool(context.Background(), p, pl, false)
	return p.calls > before
}

// Launches that keep failing are attempted 15s after the first failure,
// then 30s, 60s ... capped at 5m; the passes in between launch nothing and
// record one folded pool.scale_blocked saying why.
func TestFailedLaunchesBackOffPerPool(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	var attempts []time.Duration
	for off := time.Duration(0); off <= 1065*time.Second; off += 5 * time.Second {
		*now = start.Add(off)
		if backoffPass(s, pl, p) {
			attempts = append(attempts, off)
		}
	}
	want := []time.Duration{0, 15 * time.Second, 45 * time.Second, 105 * time.Second, 225 * time.Second,
		465 * time.Second, 765 * time.Second, 1065 * time.Second}
	if len(attempts) != len(want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempts at %v, want %v", attempts, want)
		}
	}
	*now = start.Add(1070 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("launched 5s after a failure")
	}
	if n := len(events(t, s, evLaunchFailed)); n != 1 {
		t.Errorf("%d launch_failed events, want one folded", n)
	}
	if c := queryOne[int](t, s, `SELECT count FROM pool_events WHERE type = $1`, evLaunchFailed); c != len(want) {
		t.Errorf("launch_failed count %d, want one per attempt (%d)", c, len(want))
	}
}

// A change to the pool's stored configuration (here its template) ends
// the backoff: the next pass tries the fix at once.
func TestPoolChangeResetsLaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	backoffPass(s, pl, p)
	*now = start.Add(15 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("no second attempt after 15s")
	}
	*now = start.Add(16 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted during the backoff")
	}
	execSQL(t, s, context.Background(), `UPDATE pools SET template = '{"version":2}' WHERE id = 'pool1'`)
	pl.Template = json.RawMessage(`{"version":2}`)
	*now = start.Add(17 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("a changed template waited out the backoff")
	}
	// The reset starts over: the next attempt is 15s after this failure.
	*now = start.Add(31 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted 14s after the failure")
	}
	*now = start.Add(32 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("no attempt 15s after the failure that followed the change")
	}
}

// A successful launch resets the count: the next failure waits 15s, not
// the doubled delay.
func TestSuccessfulLaunchResetsLaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	backoffPass(s, pl, p)
	p.fail = false
	*now = start.Add(15 * time.Second)
	if !backoffPass(s, pl, p) || len(p.hosts) != 1 {
		t.Fatal("no successful launch after 15s")
	}
	// The host goes away; a replacement is wanted and fails.
	s.markTerminated(context.Background(), p.hosts[0], "the provider terminated this host")
	p.fail = true
	*now = start.Add(16 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the replacement waited after a success")
	}
	*now = start.Add(30 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted 14s after the failure")
	}
	*now = start.Add(31 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the failure after a success waited longer than 15s")
	}
}

// A launch refused by the tenant's host quota calls no provider and starts
// no backoff: once the quota allows it, the next pass launches.
func TestHostQuotaDoesNotStartALaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	ctx := context.Background()
	p.fail = false
	execSQL(t, s, ctx, `UPDATE tenants SET max_hosts = 1 WHERE id = 't1'`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't1', 'other', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state) VALUES ('h-other', 'h-other', 't1', 'pool2', 'provisioning')`)
	if backoffPass(s, pl, p) {
		t.Fatal("launched past the quota")
	}
	if evs := events(t, s, evScaleBlocked); len(evs) != 1 || evs[0].Data["cause"] != causeQuota {
		t.Fatalf("scale_blocked %+v, want the quota", evs)
	}
	execSQL(t, s, ctx, `UPDATE tenants SET max_hosts = 0 WHERE id = 't1'`)
	*now = now.Add(time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("a quota refusal started a backoff")
	}
}
