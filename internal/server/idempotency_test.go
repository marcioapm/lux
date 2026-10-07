package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// submitWithKey submits a minimal Run as t1 with an idempotency key.
func submitWithKey(ctx context.Context, s *Server, key string) (*submitRunOutput, error) {
	return s.submitRun(asTenant(ctx, "t1"), &submitRunInput{IdempotencyKey: key, Body: spec.RunSpec{
		Image:    spec.Image{Ref: "alpine"},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}},
	}})
}

// checkOneRun fails unless every submit returned runID without a 5xx (or
// any error), at most one with 201, and the tenant has exactly one Run
// with key.
func checkOneRun(t *testing.T, s *Server, key, runID string, outs []*submitRunOutput, errs []error) {
	t.Helper()
	created := 0
	for i, err := range errs {
		if err != nil {
			var he *HTTPError
			if errors.As(err, &he) && he.Status >= 500 {
				t.Errorf("submit %d: %d %v", i, he.Status, err)
			} else {
				t.Errorf("submit %d: %v", i, err)
			}
			continue
		}
		if outs[i].Body.ID != runID {
			t.Errorf("submit %d: Run %s, want %s", i, outs[i].Body.ID, runID)
		}
		switch outs[i].Status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("submit %d: status %d", i, outs[i].Status)
		}
	}
	if created > 1 {
		t.Errorf("%d submits created a Run", created)
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM runs WHERE tenant_id = 't1' AND idempotency_key = $1`, []any{key}, &n)
	if n != 1 {
		t.Errorf("%d Runs with key %s, want 1", n, key)
	}
}

// Submits with one key that all miss the replay SELECT, because the first
// Run's INSERT has not committed, wait on its unique index and then
// return that Run (200), not the unique violation.
func TestConcurrentIdempotentSubmitsBlockedOnInsert(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	n := min(8, int(s.db.Pool.Config().MaxConns)-2)

	// The first Run, inserted and not yet committed.
	holder, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state, idempotency_key)
		VALUES ('run_first', 't1', '{"image": {"ref": "alpine"}, "workload": {"adapter": "generic", "command": ["true"]}}', 'submitted', 'k')`); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	outs := make([]*submitRunOutput, n)
	errs := make([]error, n)
	done := make(chan error, n)
	for i := range n {
		go func() {
			outs[i], errs[i] = submitWithKey(ctx, s, "k")
			done <- nil
		}()
	}
	// Every submit is past its SELECT and waits on the holder's row.
	for {
		var blocked int
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked)
		}); err != nil {
			t.Fatal(err)
		}
		if blocked == n {
			break
		}
		select {
		case <-done:
			t.Fatalf("a submit finished before the first Run committed")
		case <-ctx.Done():
			t.Fatalf("%d of %d submits blocked at the deadline", blocked, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, done, n)
	checkOneRun(t, s, "k", "run_first", outs, errs)
	for i, out := range outs {
		if errs[i] == nil && out.Status != http.StatusOK {
			t.Errorf("submit %d: status %d, want 200 for the first Run", i, out.Status)
		}
	}
}

// N submits with one key at once, none held: one creates the Run, every
// other returns it.
func TestConcurrentIdempotentSubmits(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	n := min(8, int(s.db.Pool.Config().MaxConns)-1)
	outs := make([]*submitRunOutput, n)
	errs := make([]error, n)
	start := make(chan struct{})
	done := make(chan error, n)
	for i := range n {
		go func() {
			<-start
			outs[i], errs[i] = submitWithKey(ctx, s, "same")
			done <- nil
		}()
	}
	close(start)
	await(t, ctx, done, n)
	var id string
	systemScan(t, s, `SELECT id FROM runs WHERE idempotency_key = 'same'`, nil, &id)
	checkOneRun(t, s, "same", id, outs, errs)
}
