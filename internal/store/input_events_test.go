package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Input events recorded before 039 are known to it: a repeat of one is
// still a conflict after the upgrade.
func TestRunInputEventsBackfill(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "038_pool_id"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`,
		`INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
			('t1', 'r1', 1, 'input.delivered', '{"requestId":"a"}'), ('t1', 'r1', 1, 'input.delivered', '{"requestId":"a"}'),
			('t1', 'r1', 1, 'input.failed', '{"requestId":"b"}'), ('t1', 'r1', 1, 'activity', '{"activity":"idle"}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, err := conn.Query(ctx, `SELECT type || ' ' || request_id FROM run_input_events ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "input.delivered a" || keys[1] != "input.failed b" {
		t.Fatalf("backfilled %q", keys)
	}
}
