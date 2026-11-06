package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

// Measure the statements issued by the scheduler, not a literal-only facsimile:
// RLS's row estimates can turn the ordinary join into quadratic work even when
// both relations have statistics and the queue is empty.
func TestHostHoursSelectorWork(t *testing.T) {
	for _, n := range []int{3300, 6600} {
		for _, analyzed := range []bool{false, true} {
			t.Run(fmt.Sprintf("hosts=%d/analyzed=%t", n, analyzed), func(t *testing.T) {
				s, _ := costFixture(t)
				ctx := context.Background()
				s.cfg.Costs.Batch = 1000
				s.cfg.Costs.Hourly = 90 * 24 * time.Hour
				hour := time.Now().UTC().Truncate(time.Hour)
				ownerExec(t, s, `ALTER TABLE hosts SET (autovacuum_enabled = false);
					ALTER TABLE cost_host_refresh SET (autovacuum_enabled = false)`)
				// Representative metadata keeps host heap width near retained production rows.
				ownerExec(t, s, `INSERT INTO hosts (id, name, tenant_id, state, registered_at, terminated_at, labels)
					SELECT 'plan-' || i, 'plan-' || i, (ARRAY['t1', 't2'])[1 + i % 2],
						'terminated', $2::timestamptz - interval '1 day', $2,
                        jsonb_build_object('metadata', (SELECT string_agg(md5((i * 100 + k)::text), '') FROM generate_series(1, 55) k))
					FROM generate_series(1, $1::int) i`, n, hour)
				ownerExec(t, s, `INSERT INTO cost_host_refresh (host_id, next_hour, retry_at)
					SELECT id, $1, $1 FROM hosts`, hour)
				if analyzed {
					ownerExec(t, s, `ANALYZE hosts; ANALYZE cost_host_refresh`)
				}
				cfg := s.db.Pool.Config()
				rec := &feedRecorder{}
				cfg.ConnConfig.Tracer = rec
				db, err := store.OpenConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				orig := s.db
				s.db = db
				err = s.updateHostHours(ctx)
				s.db = orig
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				var statements []feedStatement
				for _, q := range rec.got {
					if strings.Contains(q.sql, "SELECT h.id, greatest(") {
						statements = append(statements, q)
					}
				}
				if len(statements) != 2 {
					t.Fatalf("captured %d selectors, want both branches", len(statements))
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for branch, q := range statements {
						err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
							var role string
							var bypass bool
							if err := tx.QueryRow(ctx, `SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&role, &bypass); err != nil {
								return err
							}
							if role != "lux_app" || bypass {
								t.Fatalf("not an RLS application plan: %s bypass=%t", role, bypass)
							}
							if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
								return err
							}
							typ := "int"
							if branch == 1 {
								typ = "text[]"
							}
							if _, err := tx.Exec(ctx, "PREPARE host_selector(timestamptz,timestamptz,"+typ+") AS "+q.sql); err != nil {
								return err
							}
							defer tx.Exec(ctx, "DEALLOCATE host_selector")
							var plan []map[string]any
							args := append([]any{pgx.QueryExecModeSimpleProtocol}, q.args...)
							if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE host_selector($1,$2,$3)", args...).Scan(&plan); err != nil {
								return err
							}
							root := plan[0]["Plan"].(map[string]any)
							removed := 0.0
							var visit func(map[string]any)
							visit = func(p map[string]any) {
								for _, key := range []string{"Rows Removed by Join Filter", "Rows Removed by Filter"} {
									if v, ok := p[key].(float64); ok {
										removed += v * p["Actual Loops"].(float64)
									}
								}
								if children, ok := p["Plans"].([]any); ok {
									for _, child := range children {
										visit(child.(map[string]any))
									}
								}
							}
							visit(root)
							blocks := root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64)
							t.Logf("%s branch=%d removed=%.0f buffers=%.0f", mode, branch, removed, blocks)
							if removed > float64(n*8) || blocks > float64(n*12) || root["Actual Rows"].(float64) != 0 {
								encoded, _ := json.Marshal(plan)
								t.Logf("plan=%s", encoded)
								t.Errorf("selector work not linear or empty: removed=%.0f buffers=%.0f rows=%v", removed, blocks, root["Actual Rows"])
							}
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
}
