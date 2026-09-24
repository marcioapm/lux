package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// testServer is a Server on a fresh database, for what e2e tests cannot
// wait for (rollups close minute and hour buckets). Needs LUX_TEST_PG, as
// the store tests do; skipped otherwise.
func testServer(t *testing.T) *Server {
	t.Helper()
	admin := os.Getenv("LUX_TEST_PG")
	if admin == "" {
		t.Skip("LUX_TEST_PG not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	name := "lux_unit_" + ids.New("")[1:]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg := conn.Config()
	owner := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, fmt.Sprintf("postgres://lux_app:lux_app@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		conn.Exec(ctx, "DROP DATABASE "+name)
		conn.Close(ctx)
	})
	return New(Config{}, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRollupHistory(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`); err != nil {
			return err
		}
		// Two raw samples in each of two minutes, and system samples
		// counting starts.
		for i, at := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
			if _, err := tx.Exec(ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds, mem_bytes, placements)
				VALUES ('h1', 0, $1, $2, $3, 1)`, hour.Add(at), float64(i*10), int64(100*(i+1))); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO system_samples (tenant_id, res, at, runs, started) VALUES ('', 0, $1, $2, 1)`,
				hour.Add(at), map[string]int{"running": i}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Twice: a second pass must not add or change anything.
	for range 2 {
		if err := s.rollupHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		at    time.Time
		cpu   float64
		mem   int64
		start int
		runs  map[string]int
	}
	var mins []row
	var hours []row
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT h.at, h.cpu_seconds, h.mem_bytes, s.started, s.runs FROM host_samples h
			JOIN system_samples s ON s.at = h.at AND s.res = h.res AND s.tenant_id = '' WHERE h.res = 60 ORDER BY h.at`)
		mins, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.at, &x.cpu, &x.mem, &x.start, &x.runs)
		})
		if err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, `SELECT h.at, h.cpu_seconds, h.mem_bytes, s.started, s.runs FROM host_samples h
			JOIN system_samples s ON s.at = h.at AND s.res = h.res AND s.tenant_id = '' WHERE h.res = 3600 ORDER BY h.at`)
		hours, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.at, &x.cpu, &x.mem, &x.start, &x.runs)
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mins) != 2 {
		t.Fatalf("minute rollups: %+v", mins)
	}
	// Counters: the last (max); levels: the mean; flows: the sum; states: the last.
	if m := mins[0]; !m.at.Equal(hour) || m.cpu != 10 || m.mem != 150 || m.start != 2 || m.runs["running"] != 1 {
		t.Fatalf("first minute: %+v", m)
	}
	if m := mins[1]; m.cpu != 30 || m.mem != 350 || m.runs["running"] != 3 {
		t.Fatalf("second minute: %+v", m)
	}
	if len(hours) != 1 || !hours[0].at.Equal(hour) || hours[0].start != 4 || hours[0].cpu != 30 || hours[0].mem != 250 {
		t.Fatalf("hour rollup: %+v", hours)
	}
}
