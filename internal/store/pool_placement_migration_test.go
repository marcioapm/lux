package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

// 073: a pool from before it has no placement (its default); spread and
// pack round-trip; any other value is refused by the CHECK.
func TestPoolPlacementMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "072_host_disk_samples"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool_old', 'old', 'static')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var placement *string
	if err := conn.QueryRow(ctx, `SELECT placement FROM pools WHERE id = 'pool_old'`).Scan(&placement); err != nil || placement != nil {
		t.Fatalf("legacy pool: placement %v (%v), want NULL", placement, err)
	}
	for _, v := range []string{"spread", "pack"} {
		if _, err := conn.Exec(ctx, `UPDATE pools SET placement = $1 WHERE id = 'pool_old'`, v); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		var got string
		if err := conn.QueryRow(ctx, `SELECT placement FROM pools WHERE id = 'pool_old'`).Scan(&got); err != nil || got != v {
			t.Fatalf("round trip %s: %q %v", v, got, err)
		}
	}
	_, err = conn.Exec(ctx, `UPDATE pools SET placement = 'random' WHERE id = 'pool_old'`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("placement 'random': %v, want a check violation", err)
	}
}
