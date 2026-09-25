package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func execSQL(t *testing.T, s *Server, ctx context.Context, q string, args ...any) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

// exitRequested maps every host id to whether exit_requested_at is set.
func exitRequested(t *testing.T, s *Server, ctx context.Context) map[string]bool {
	t.Helper()
	var got map[string]bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, exit_requested_at IS NOT NULL FROM hosts`)
		if err != nil {
			return err
		}
		got = map[string]bool{}
		for rows.Next() {
			var id string
			var requested bool
			if err := rows.Scan(&id, &requested); err != nil {
				return err
			}
			got[id] = requested
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A static host drained for outdated binaries, idle and with nothing to
// upload, is told to exit exactly once: a second pass must not enqueue a
// second exit message.
func TestReapOutdatedStaticHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, draining, state_reason, drain_causes) VALUES
		('h1', 'h1', 'draining', true, $1, ARRAY[$2]),
		('h2', 'h2', 'draining', true, 'some other reason', ARRAY['manual']),
		('h3', 'h3', 'ready', false, '', '{}')`, outdatedBinariesReason, causeOutdated)

	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}

	got := exitRequested(t, s, ctx)
	if !got["h1"] || got["h2"] || got["h3"] {
		t.Errorf("exit_requested = %v, want only h1 (outdated, idle), not h2 (other reason) or h3 (not draining)", got)
	}

	if n := exitMessageCount(t, s, ctx, "h1"); n != 1 {
		t.Fatalf("host_messages for h1: %d, want 1", n)
	}

	// A second pass finds h1 already requested (and not yet stale): no
	// second message.
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, "h1"); n != 1 {
		t.Fatalf("host_messages for h1 after a second pass: %d, want still 1", n)
	}
}

// A host whose exit went unacked past the retry window (the runner
// ignored it, or the fetch failed and it kept its old binaries) is sent a
// second exit: still outdated and idle, exit_requested_at just old.
func TestReapOutdatedStaticHostsRetriesAStaleExit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, draining, state_reason, drain_causes, exit_requested_at) VALUES
		('h1', 'h1', 'draining', true, $1, ARRAY[$2], now() - interval '11 minutes')`, outdatedBinariesReason, causeOutdated)

	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, "h1"); n != 1 {
		t.Fatalf("host_messages for h1 after a stale exit: %d, want 1 (re-sent)", n)
	}
	if got := exitRequested(t, s, ctx); !got["h1"] {
		t.Error("exit_requested_at was not bumped on retry")
	}
}

// reapOutdatedStaticHosts must not exit a host that still has a live
// placement, an unshipped blob, that is provisioned (the pool's own
// replace path handles those instead), or that also carries "manual" (an
// operator's drain, or pools rm): the operator owns it now, and it only
// updates once they undrain it or restart it by hand.
func TestReapOutdatedStaticHostsExclusions(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, draining, state_reason, drain_causes) VALUES
		('h-busy', 'h-busy', 'draining', true, $1, ARRAY[$2]),
		('h-blob', 'h-blob', 'draining', true, $1, ARRAY[$2]),
		('h-manual', 'h-manual', 'draining', true, $1, ARRAY[$2, 'manual'])`, outdatedBinariesReason, causeOutdated)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, draining, state_reason, drain_causes, provision_requested_at) VALUES
		('h-prov', 'h-prov', 'draining', true, $1, ARRAY[$2], now())`, outdatedBinariesReason, causeOutdated)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h-busy', 1, 'running')`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, host_id) VALUES
		('b1', 't1', 'r1', 1, 'output', 'out', 'host', 'h-blob')`)

	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}

	requested := exitRequested(t, s, ctx)
	if requested["h-busy"] {
		t.Error("h-busy (live placement): exit was requested, want held back")
	}
	if requested["h-blob"] {
		t.Error("h-blob (unshipped blob): exit was requested, want held back")
	}
	if requested["h-prov"] {
		t.Error("h-prov (provisioned): exit was requested, want left to the pool's replace path")
	}
	if requested["h-manual"] {
		t.Error("h-manual (also manually drained): exit was requested, want held back for the operator")
	}
}
