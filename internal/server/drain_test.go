package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// drainState reads back a host's draining flag and its live placement's
// stop_requested_at (empty runID: only the host is checked).
func drainState(t *testing.T, s *Server, ctx context.Context, hostID, runID string) (draining bool, stopRequested bool) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT draining FROM hosts WHERE id = $1`, hostID).Scan(&draining); err != nil {
			return err
		}
		if runID == "" {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT stop_requested_at IS NOT NULL FROM placements WHERE run_id = $1`, runID).Scan(&stopRequested)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// A plain drainHost (forceEvict false) cordons the host but leaves its
// live placement alone: no stop is requested.
func TestDrainHostWithoutForceEvictLeavesRunAlone(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	exec(`INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`)
	exec(`INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running')`)
	exec(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &drainHostInput{}
	in.ID = "h1"
	if _, err := s.drainHost(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, stopRequested := drainState(t, s, context.Background(), "h1", "r1")
	if !draining {
		t.Error("host was not cordoned")
	}
	if stopRequested {
		t.Error("a plain drain requested a stop; want the running Run left alone")
	}
}

// forceEvict on drainHost stops the host's live placement, and does so
// even when the host is already draining.
func TestDrainHostWithForceEvictStopsRun(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	exec(`INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`)
	exec(`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	exec(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})

	// First a plain drain (as the console's dialog would do by default).
	in := &drainHostInput{}
	in.ID = "h1"
	if _, err := s.drainHost(ctx, in); err != nil {
		t.Fatal(err)
	}
	if draining, stopRequested := drainState(t, s, context.Background(), "h1", "r1"); !draining || stopRequested {
		t.Fatalf("after plain drain: draining=%v stopRequested=%v", draining, stopRequested)
	}

	// forceEvict on the already-draining host must still evict its Run.
	in2 := &drainHostInput{}
	in2.ID = "h1"
	in2.Body.ForceEvict = true
	if _, err := s.drainHost(ctx, in2); err != nil {
		t.Fatal(err)
	}
	draining, stopRequested := drainState(t, s, context.Background(), "h1", "r1")
	if !draining || !stopRequested {
		t.Fatalf("after forceEvict on an already-draining host: draining=%v stopRequested=%v, want both true", draining, stopRequested)
	}
}

// deletePool without forceEvict cordons the pool's provisioned hosts and
// leaves their live Runs running; its own hosts are picked up by the
// provisioner's replace path once idle (not exercised here).
func TestDeletePoolWithoutForceEvictLeavesRunsAlone(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	exec(`INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	exec(`INSERT INTO hosts (id, tenant_id, name, pool, state, provider_id) VALUES ('h1', 't1', 'h1', 'burst', 'ready', 'i-123')`)
	exec(`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	exec(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &deletePoolInput{Name: "burst"}
	if _, err := s.deletePool(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, stopRequested := drainState(t, s, context.Background(), "h1", "r1")
	if !draining {
		t.Error("the pool's host was not cordoned")
	}
	if stopRequested {
		t.Error("deletePool without forceEvict requested a stop; want the running Run left alone")
	}
	var retired bool
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT retired FROM pools WHERE id = 'pool1'`).Scan(&retired)
	}); err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Error("the pool was not retired")
	}
}

// deletePool with forceEvict also stops the pool's hosts' live Runs.
func TestDeletePoolWithForceEvictStopsRuns(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	exec(`INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	exec(`INSERT INTO hosts (id, tenant_id, name, pool, state, provider_id) VALUES ('h1', 't1', 'h1', 'burst', 'ready', 'i-123')`)
	exec(`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	exec(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &deletePoolInput{Name: "burst", ForceEvict: true}
	if _, err := s.deletePool(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, stopRequested := drainState(t, s, context.Background(), "h1", "r1")
	if !draining || !stopRequested {
		t.Fatalf("deletePool with forceEvict: draining=%v stopRequested=%v, want both true", draining, stopRequested)
	}
}

// A static (non-provisioned) host in a deleted pool is cordoned like any
// other: deletePool never touches placements outside the pool's own hosts.
func TestDeletePoolOnlyTouchesItsOwnHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	exec(`INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	exec(`INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('h-other', 't1', 'h-other', 'default', 'ready')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &deletePoolInput{Name: "burst", ForceEvict: true}
	if _, err := s.deletePool(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, _ := drainState(t, s, context.Background(), "h-other", "")
	if draining {
		t.Error("deletePool cordoned a host outside the deleted pool")
	}
}
