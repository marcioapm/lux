package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

// This SELECT starts with a newline, unlike the SELECT-space statements
// feedRecorder records. Keep the SQL and parameters actually issued by poolState.
type poolActivityRecorder struct {
	mu  sync.Mutex
	got []feedStatement
}

func (r *poolActivityRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "SELECT\n") && strings.Contains(d.SQL, "EXISTS (SELECT 1 FROM hosts h JOIN placements p") {
		r.mu.Lock()
		r.got = append(r.got, feedStatement{d.SQL, slices.Clone(d.Args)})
		r.mu.Unlock()
	}
	return ctx
}

func (*poolActivityRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func capturePoolActivity(t *testing.T, s *Server, pl poolRow) feedStatement {
	t.Helper()
	ctx := context.Background()
	cfg := s.db.Pool.Config()
	rec := &poolActivityRecorder{}
	cfg.ConnConfig.Tracer = rec
	db, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	orig := s.db
	s.db = db
	defer func() { s.db = orig; db.Close() }()
	var st poolState
	if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("poolState issued %d activity queries, want exactly one", len(rec.got))
	}
	t.Logf("actual poolState activity SQL: %s; args=%v", rec.got[0].sql, rec.got[0].args)
	return rec.got[0]
}

func TestPoolActivitySemantics(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES
		('amd', 't1', 'amd', 'ec2'), ('arm', 't1', 'arm', 'ec2'), ('default', 't2', 'default', 'ec2');
		INSERT INTO hosts (id, tenant_id, pool_id, name, state) VALUES
		('h1', 't1', 'amd', 'h1', 'terminated'), ('h2', 't2', 'default', 'h2', 'ready');
		INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('r1', 't1', '{}', 'exited'), ('r2', 't2', '{}', 'running');
		INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		VALUES ('live', 't2', 'r2', 'h2', 1, 'running')`)
	actual := capturePoolActivity(t, s, poolRow{ID: "amd", TenantID: new("t1"), ScaleDownAfterS: new(600)})
	for _, scope := range []string{"system", "t1", "t2"} {
		t.Run(scope, func(t *testing.T) {
			tx, err := s.db.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			setting, value := "lux.tenant_id", scope
			if scope == "system" {
				setting, value = "lux.system", "on"
			}
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
				t.Fatal(err)
			}
			check := func(pool string, want bool) {
				t.Helper()
				var got bool
				if err := tx.QueryRow(ctx, actual.sql, pool, actual.args[1]).Scan(&got); err != nil || got != want {
					t.Fatalf("pool=%s active=%t want=%t err=%v", pool, got, want, err)
				}
			}
			check("default", scope != "t1")
			check("amd", false)
			check("arm", false)
			// Tenant t2 cannot mutate or observe t1's activity.
			if scope == "t2" {
				return
			}
			for _, c := range []struct {
				name, state, created, ended string
				spot, want                  bool
			}{
				{"live", "running", "now() - interval '1 day'", "NULL", false, true},
				{"recent-ended", "exited", "now() - interval '1 day'", "now()", false, true},
				{"recent-created-exited", "exited", "now()", "NULL", false, true},
				{"old-created-exited", "exited", "now() - interval '1 day'", "NULL", false, false},
				{"spot-live", "running", "now()", "NULL", true, false},
				{"spot-ended", "exited", "now() - interval '1 day'", "now()", true, false},
				{"spot-created", "exited", "now()", "NULL", true, false},
				{"ended-boundary", "exited", "now() - interval '1 day'", "now() - interval '10 minutes'", false, false},
				{"ended-epsilon", "exited", "now() - interval '1 day'", "now() - interval '10 minutes' + interval '1 microsecond'", false, true},
				{"created-boundary", "exited", "now() - interval '10 minutes'", "NULL", false, false},
				{"created-epsilon", "exited", "now() - interval '10 minutes' + interval '1 microsecond'", "NULL", false, true},
			} {
				t.Run(c.name, func(t *testing.T) {
					if _, err := tx.Exec(ctx, `UPDATE runs SET spec = jsonb_build_object('spot', $1::boolean) WHERE id = 'r1'`, c.spot); err != nil {
						t.Fatal(err)
					}
					q := fmt.Sprintf(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, ended_at)
						VALUES ('test', 't1', 'r1', 'h1', 1, $1, %s, %s)`, c.created, c.ended)
					if _, err := tx.Exec(ctx, q, c.state); err != nil {
						t.Fatal(err)
					}
					check("amd", c.want) // Terminated hosts still associate historical placements with their pool.
					if _, err := tx.Exec(ctx, `DELETE FROM placements WHERE id = 'test'`); err != nil {
						t.Fatal(err)
					}
				})
			}
			if _, err := tx.Exec(ctx, `UPDATE runs SET spec = '{}' WHERE id = 'r1';
				INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
				VALUES ('transition', 't1', 'r1', 'h1', 1, 'exited')`); err != nil {
				t.Fatal(err)
			}
			// Inspect predicate membership through the real partial-index paths,
			// then end the placement and check it moves to the other index.
			for _, ended := range []bool{false, true} {
				if ended {
					if _, err := tx.Exec(ctx, `UPDATE placements SET ended_at = now() WHERE id = 'transition'`); err != nil {
						t.Fatal(err)
					}
				}
				check("amd", true)
				var createdMember, endedMember bool
				if err := tx.QueryRow(ctx, `SELECT
					EXISTS (SELECT 1 FROM placements WHERE id = 'transition' AND ended_at IS NULL),
					EXISTS (SELECT 1 FROM placements WHERE id = 'transition' AND ended_at IS NOT NULL)`).Scan(&createdMember, &endedMember); err != nil || createdMember == ended || endedMember != ended {
					t.Fatalf("transition ended=%t created=%t ended-index=%t err=%v", ended, createdMember, endedMember, err)
				}
			}
		})
	}
}

// The guard is independent of elapsed time and the complete planner tree.
// Empty recent ranges should cost a few pages, not a walk of retained history.
const poolActivityBufferBudget = 100

func poolActivityPlan(t *testing.T, tx pgx.Tx, actual feedStatement, pool, mode string) float64 {
	t.Helper()
	ctx := context.Background()
	var role, jit, thresholds, system string
	var bypass bool
	if err := tx.QueryRow(ctx, `SELECT current_user, rolsuper OR rolbypassrls, current_setting('jit'),
		current_setting('jit_above_cost') || '/' || current_setting('jit_inline_above_cost') || '/' || current_setting('jit_optimize_above_cost'),
		current_setting('lux.system', true) FROM pg_roles WHERE rolname = current_user`).Scan(&role, &bypass, &jit, &thresholds, &system); err != nil {
		t.Fatal(err)
	}
	if role != "lux_app" || bypass || jit != "on" || thresholds != "100000/500000/500000" {
		t.Fatalf("need app RLS and default JIT: role=%s bypass=%t jit=%s thresholds=%s", role, bypass, jit, thresholds)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "PREPARE pool_activity AS "+actual.sql); err != nil {
		t.Fatal(err)
	}
	defer tx.Exec(ctx, "DEALLOCATE pool_activity")
	var plan []map[string]any
	// Values originate in this test/captured configuration, not external input.
	q := fmt.Sprintf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE pool_activity('%s', '%s')", pool, actual.args[1])
	// Do not let pgx cache EXPLAIN EXECUTE across redefinitions of PREPARE.
	if err := tx.QueryRow(ctx, q, pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	root := plan[0]["Plan"].(map[string]any)
	if pool == "amd" && mode == "force_generic_plan" {
		encoded, _ := json.Marshal(plan)
		t.Logf("diagnostic plan=%s", encoded)
	}
	blocks := root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64)
	var indexes []string
	var removed float64
	var walk func(map[string]any)
	walk = func(n map[string]any) {
		if pool == "amd" && mode == "force_generic_plan" && (n["Relation Name"] == "placements" || n["Index Name"] == "placements_recent_ended") {
			t.Logf("scan type=%v index=%v cond=%v filter=%v rows=%v loops=%v cost=%v/%v", n["Node Type"], n["Index Name"], n["Index Cond"], n["Filter"], n["Plan Rows"], n["Actual Loops"], n["Startup Cost"], n["Total Cost"])
		}
		if name, ok := n["Index Name"].(string); ok {
			indexes = append(indexes, name)
		}
		if rows, ok := n["Rows Removed by Filter"].(float64); ok {
			removed += rows * n["Actual Loops"].(float64)
		}
		if children, ok := n["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(root)
	t.Logf("pool=%s mode=%s role=%s system=%s buffers=%.0f removed=%.0f execution_ms=%v indexes=%v", pool, mode, role, system, blocks, removed, plan[0]["Execution Time"], indexes)
	if _, jit := plan[0]["JIT"]; jit {
		t.Error("pool activity unexpectedly compiled JIT with default settings")
	}
	return blocks
}

func TestPoolActivityHistoryWork(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES
		('amd', 't1', 'amd', 'ec2'), ('arm', 't1', 'arm', 'ec2'), ('default', 't1', 'default', 'ec2')`)
	actual := capturePoolActivity(t, s, poolRow{ID: "amd", TenantID: new("t1"), ScaleDownAfterS: new(600)})
	cfg, err := pgx.ParseConfig(os.Getenv("LUX_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = s.db.Pool.Config().ConnConfig.Database
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	var version string
	if err := owner.QueryRow(ctx, `SHOW server_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("PostgreSQL %s; default JIT settings are asserted on each actual app-role plan", version)
	for _, size := range []int{3400, 6800} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			start, placements := 1, 28000
			if size == 6800 {
				start, placements = 3401, 56000
			}
			// Varying 1760-byte labels and populated specs/templates reproduce
			// historical heap width; no RLS or planner settings are bypassed.
			ownerExec(t, s, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, terminated_at,
				launch_template, labels, capacity, versions, caches)
				SELECT 'h' || i, 't1', 'h' || i, CASE WHEN (i - 1) % 3400 < 1826 THEN 'amd' ELSE 'default' END,
				CASE WHEN i = 3400 THEN 'ready' ELSE 'terminated' END, now() - interval '1 day',
				CASE WHEN i <> 3400 THEN now() - interval '1 hour' END,
				'{"region":"eu-west-1","instanceType":"m7i.large","image":"ami-fixture"}',
				jsonb_build_object('metadata', (SELECT string_agg(md5((i * 100 + k)::text), '') FROM generate_series(1,55) k)),
				'{"cpus":2,"memory":8589934592}', '{"runner":"fixture"}', '{"images":["fixture"]}'
				FROM generate_series($1::int, $2::int) i`, start, size)
			first := 1
			if size == 6800 {
				first = 28001
			}
			ownerExec(t, s, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch)
				SELECT 'r' || i, 't1', '{"image":"fixture","command":["true"],"resources":{"cpus":1,"memory":"1GiB"}}', 'exited', 1
				FROM generate_series($1::int, $2::int) i;
				`, first, placements)
			ownerExec(t, s, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, ended_at, resources)
				SELECT 'p' || i, 't1', 'r' || i, 'h' || (1 + (i - 1) % $3::int), 1, 'exited', now() - interval '1 day',
				now() - interval '1 hour', '{"cpus":1,"memory":1073741824,"disk":10737418240}'
				FROM generate_series($1::int, $2::int) i`, first, placements, size)
			if size == 3400 {
				ownerExec(t, s, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
					('live', 't1', '{}', 'running', 1), ('extra', 't1', '{}', 'exited', 0);
					INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at)
					VALUES ('live', 't1', 'live', 'h3400', 1, 'running', now() - interval '1 day')`)
			}
			ownerExec(t, s, `ANALYZE hosts; ANALYZE runs; ANALYZE placements; ANALYZE pools`)
			var hosts, runs, count int
			if err := owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM hosts), (SELECT count(*) FROM runs), (SELECT count(*) FROM placements)`).Scan(&hosts, &runs, &count); err != nil || hosts != size || runs != placements+2 || count != placements+1 {
				t.Fatalf("fixture hosts=%d runs=%d placements=%d err=%v", hosts, runs, count, err)
			}
			var stats string
			if err := owner.QueryRow(ctx, `SELECT string_agg(relname || ':' || n_live_tup || ':' || coalesce(last_analyze::text, ''), ', ' ORDER BY relname)
				FROM pg_stat_user_tables WHERE relname IN ('hosts', 'runs', 'placements', 'pools')`).Scan(&stats); err != nil {
				t.Fatal(err)
			}
			t.Logf("fixture hosts=%d runs=%d placements=%d statistics=%s", hosts, runs, count, stats)
			for _, scope := range []string{"system", "t1", "t2"} {
				for _, mode := range []string{"force_generic_plan", "force_custom_plan"} {
					for _, missing := range []bool{false, true} {
						// DDL is transactional and confined to this test's database.
						tx, err := owner.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						func() {
							defer tx.Rollback(ctx)
							if missing {
								if _, err := tx.Exec(ctx, `DROP INDEX placements_recent_ended; DROP INDEX placements_recent_created`); err != nil {
									t.Fatal(err)
								}
							}
							setting, value := "lux.tenant_id", scope
							if scope == "system" {
								setting, value = "lux.system", "on"
							}
							if _, err := tx.Exec(ctx, `SET LOCAL ROLE lux_app`); err != nil {
								t.Fatal(err)
							}
							if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
								t.Fatal(err)
							}
							for _, pool := range []string{"amd", "arm", "default"} {
								var active bool
								if err := tx.QueryRow(ctx, actual.sql, pool, actual.args[1]).Scan(&active); err != nil || active != (pool == "default" && scope != "t2") {
									t.Fatalf("fixture activity pool=%s scope=%s active=%t err=%v", pool, scope, active, err)
								}
								t.Logf("size=%d scope=%s missing_indexes=%t", size, scope, missing)
								blocks := poolActivityPlan(t, tx, actual, pool, mode)
								if !missing && blocks > poolActivityBufferBudget {
									t.Errorf("indexed activity walked history: %.0f buffers > %d", blocks, poolActivityBufferBudget)
								}
								if missing && scope != "t2" && pool == "amd" && blocks <= poolActivityBufferBudget {
									t.Errorf("negative control did not reject missing indexes: %.0f buffers <= %d", blocks, poolActivityBufferBudget)
								}
							}
							// PREPARE is session-scoped, even across rollback.
						}()
					}
				}
			}
			// Warm isolated builds and small write batches are risk evidence,
			// not assertions about production migration latency or write SLAs.
			tx, err := owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `DROP INDEX placements_recent_ended; DROP INDEX placements_recent_created`); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if _, err := tx.Exec(ctx, `CREATE INDEX placements_recent_ended ON placements (ended_at) WHERE ended_at IS NOT NULL;
				CREATE INDEX placements_recent_created ON placements (created_at) WHERE ended_at IS NULL`); err != nil {
				t.Fatal(err)
			}
			var bytes int64
			if err := tx.QueryRow(ctx, `SELECT pg_relation_size('placements_recent_ended') + pg_relation_size('placements_recent_created')`).Scan(&bytes); err != nil {
				t.Fatal(err)
			}
			t.Logf("warm isolated two-index build=%s combined_bytes=%d (normal CREATE INDEX blocks writers until transaction ends)", time.Since(started), bytes)
		})
	}
}
