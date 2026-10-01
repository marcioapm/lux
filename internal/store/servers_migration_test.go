package store_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// A Run's servers from before 047 become servers of their own: an id, a
// host <name>-<8 of the id>, attached to their Run, lifetime run, waking
// never; their process state as it was. Existing events stay the Run's.
func TestWakeableServersMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "042_dashboard_indexes"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('run_a', 't1', '{}', 'running', 1), ('run_b', 't1', '{}', 'stopped', 1),
			('run_done', 't1', '{}', 'succeeded', 1), ('run_gone', 't1', '{}', 'cancelled', 1), ('run_bad', 't1', '{}', 'failed', 1)`,
		`INSERT INTO run_servers (tenant_id, run_id, name, port, command, from_spec, state, stop_reason) VALUES
			('t1', 'run_a', 'web', 3000, '["serve"]', true, 'ready', NULL),
			('t1', 'run_b', 'web', 3000, NULL, false, 'stopped', 'stopped'),
			('t1', 'run_done', 'web', 3000, NULL, true, 'stopped', NULL),
			('t1', 'run_gone', 'web', 3000, NULL, true, 'stopped', NULL),
			('t1', 'run_bad', 'web', 3000, NULL, true, 'stopped', NULL)`,
		`INSERT INTO run_events (tenant_id, run_id, type, data) VALUES ('t1', 'run_a', 'server.state', '{"name": "web"}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	// Those of succeeded and cancelled Runs are gone; a failed Run's stays.
	var failed, finished int
	if err := conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE run_id = 'run_bad'),
		count(*) FILTER (WHERE run_id IN ('run_done', 'run_gone')) FROM run_servers`).Scan(&failed, &finished); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || finished != 0 {
		t.Fatalf("kept: %d of the failed Run, %d of the finished ones", failed, finished)
	}
	rows, err := conn.Query(ctx, `SELECT id, host, run_id, lifetime, wake, state, coalesce(stop_reason, ''), from_spec FROM run_servers
		WHERE run_id IN ('run_a', 'run_b') ORDER BY run_id`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID, Host, RunID, Lifetime, Wake, State, StopReason string
		FromSpec                                           bool
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	idRe := regexp.MustCompile(`^srv_[a-z2-7]{16}$`)
	if len(got) != 2 || got[0].ID == got[1].ID || got[0].Host == got[1].Host {
		t.Fatalf("migrated: %+v", got)
	}
	for i, want := range []row{{RunID: "run_a", State: "ready", FromSpec: true}, {RunID: "run_b", State: "stopped", StopReason: "stopped"}} {
		g := got[i]
		if !idRe.MatchString(g.ID) || g.Host != "web-"+g.ID[4:12] || g.RunID != want.RunID || g.Lifetime != "run" || g.Wake != "never" ||
			g.State != want.State || g.StopReason != want.StopReason || g.FromSpec != want.FromSpec {
			t.Errorf("row %d: %+v", i, g)
		}
	}
	// Hosts are unique; a new row without one gets its own.
	if _, err := conn.Exec(ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, host) VALUES ('t1', 'run_b', 'x', 1, $1)`, got[0].Host); err == nil {
		t.Fatal("a taken host was taken again")
	}
	var host string
	if err := conn.QueryRow(ctx, `INSERT INTO run_servers (tenant_id, name, port) VALUES ('t1', 'api', 1) RETURNING host`).Scan(&host); err != nil ||
		!regexp.MustCompile(`^api-[a-z2-7]{8}$`).MatchString(host) {
		t.Fatalf("generated host: %q %v", host, err)
	}
	// A server's event without a Run; never an event of neither.
	if _, err := conn.Exec(ctx, `INSERT INTO run_events (tenant_id, server_id, type) VALUES ('t1', 'srv_x', 'server.created')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO run_events (tenant_id, type) VALUES ('t1', 'x')`); err == nil {
		t.Fatal("an event of nothing")
	}
}
