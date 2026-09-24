package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// A static host drained for outdated binaries, idle and with nothing to
// upload, is told to exit exactly once: a second pass must not enqueue a
// second exit message.
func TestReapOutdatedStaticHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO hosts (id, name, state, draining, state_reason) VALUES
		('h1', 'h1', 'draining', true, $1),
		('h2', 'h2', 'draining', true, 'some other reason'),
		('h3', 'h3', 'ready', false, '')`, outdatedBinariesReason)

	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}

	got := map[string]*string{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, exit_requested_at IS NOT NULL FROM hosts ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var requested bool
			if err := rows.Scan(&id, &requested); err != nil {
				return err
			}
			s := "no"
			if requested {
				s = "yes"
			}
			got[id] = &s
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if *got["h1"] != "yes" {
		t.Errorf("h1 (outdated, idle): exit_requested = %v, want set", *got["h1"])
	}
	if *got["h2"] != "no" {
		t.Errorf("h2 (draining for another reason): exit_requested = %v, want unset", *got["h2"])
	}
	if *got["h3"] != "no" {
		t.Errorf("h3 (not draining): exit_requested = %v, want unset", *got["h3"])
	}

	var msgs int
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_messages WHERE host_id = 'h1' AND type = $1`, proto.MsgExit).Scan(&msgs)
	})
	if err != nil {
		t.Fatal(err)
	}
	if msgs != 1 {
		t.Fatalf("host_messages for h1: %d, want 1", msgs)
	}

	// A second pass finds h1 already requested: no second message.
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_messages WHERE host_id = 'h1' AND type = $1`, proto.MsgExit).Scan(&msgs)
	})
	if err != nil {
		t.Fatal(err)
	}
	if msgs != 1 {
		t.Fatalf("host_messages for h1 after a second pass: %d, want still 1", msgs)
	}
}
