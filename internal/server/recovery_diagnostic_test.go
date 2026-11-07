package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Disposable diagnostic harness. The recovered regression tests remain intact.
func recoveryCandidate(sql, candidate string) string {
	if candidate == "original" {
		return sql
	}
	if candidate == "ended-only" {
		return strings.Replace(sql,
			"OR EXISTS (SELECT 1 FROM hosts h JOIN placements p ON p.host_id = h.id JOIN runs r ON r.id = p.run_id\n\t\t\t\tWHERE h.pool_id = $1 AND p.ended_at > now() - $2::interval",
			"OR EXISTS (WITH recent AS MATERIALIZED (SELECT host_id, run_id FROM placements WHERE ended_at > now() - $2::interval)\n\t\t\t\tSELECT 1 FROM recent p JOIN hosts h ON h.id = p.host_id JOIN runs r ON r.id = p.run_id WHERE h.pool_id = $1", 1)
	}
	if candidate == "materialized" {
		return strings.Replace(recoveryCandidate(sql, "ended-only"),
			"OR EXISTS (SELECT 1 FROM hosts h JOIN placements p ON p.host_id = h.id JOIN runs r ON r.id = p.run_id\n\t\t\t\tWHERE h.pool_id = $1 AND p.ended_at IS NULL AND p.created_at > now() - $2::interval",
			"OR EXISTS (WITH recent AS MATERIALIZED (SELECT host_id, run_id FROM placements WHERE ended_at IS NULL AND created_at > now() - $2::interval)\n\t\t\t\tSELECT 1 FROM recent p JOIN hosts h ON h.id = p.host_id JOIN runs r ON r.id = p.run_id WHERE h.pool_id = $1", 1)
	}
	panic(candidate)
}

func TestRecoveryDiagnostic(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants(id,name) VALUES ('t1','t1'),('t2','t2');
		INSERT INTO pools(id,tenant_id,name,provider) VALUES ('amd','t1','amd','ec2'),('arm','t1','arm','ec2'),('default','t1','default','ec2')`)
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
	out := os.Getenv("RECOVERY_OUT")
	if out == "" {
		t.Fatal("RECOVERY_OUT required")
	}
	size := 3400
	if os.Getenv("RECOVERY_DOUBLE") == "1" {
		size = 6800
	}
	ownerExec(t, s, `INSERT INTO hosts(id,tenant_id,name,pool_id,state,provision_requested_at,terminated_at,launch_template,labels,capacity,versions,caches)
		SELECT 'h'||i,'t1','h'||i,CASE WHEN (i-1)%3400<1826 THEN 'amd' ELSE 'default' END,
		CASE WHEN i=3400 THEN 'ready' ELSE 'terminated' END,now()-interval '1 day',CASE WHEN i<>3400 THEN now()-interval '1 hour' END,
		'{"region":"eu-west-1","instanceType":"m7i.large","image":"ami-fixture"}',
		jsonb_build_object('metadata',(SELECT string_agg(md5((i*100+k)::text),'') FROM generate_series(1,55) k)),
		'{"cpus":2,"memory":8589934592}','{"runner":"fixture"}','{"images":["fixture"]}' FROM generate_series(1,$1::int) i`, size)
	ownerExec(t, s, `INSERT INTO runs(id,tenant_id,spec,state,current_epoch)
		SELECT 'r'||i,'t1','{"image":"fixture","command":["true"],"resources":{"cpus":1,"memory":"1GiB"}}','exited',1 FROM generate_series(1,$1::int) i`, size*28000/3400)
	ownerExec(t, s, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,created_at,ended_at,resources)
		SELECT 'p'||i,'t1','r'||i,'h'||(1+(i-1)%$1::int),1,'exited',now()-interval '1 day',now()-interval '1 hour',
		'{"cpus":1,"memory":1073741824,"disk":10737418240}' FROM generate_series(1,$2::int) i`, size, size*28000/3400)
	ownerExec(t, s, `INSERT INTO runs(id,tenant_id,spec,state,current_epoch) VALUES ('live','t1','{}','running',1);
		INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,created_at) VALUES ('live','t1','live','h3400',1,'running',now()-interval '1 day')`)
	if os.Getenv("RECOVERY_UNENDED") == "1" {
		ownerExec(t, s, `UPDATE placements SET ended_at=NULL WHERE id <> 'live' AND substring(id from 2)::int % 2 = 0`)
	}
	if os.Getenv("RECOVERY_DENSE_RECENT") == "1" {
		ownerExec(t, s, `UPDATE placements SET ended_at=now() WHERE ended_at IS NOT NULL; ANALYZE hosts; ANALYZE placements; ANALYZE runs`)
		for _, candidate := range []string{"original", "materialized"} {
			for _, scope := range []string{"system", "t1", "t2"} {
				tx, err := owner.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, `SET LOCAL ROLE lux_app; SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
					t.Fatal(err)
				}
				setting, value := "lux.tenant_id", scope
				if scope == "system" {
					setting, value = "lux.system", "on"
				}
				if _, err = tx.Exec(ctx, `SELECT set_config($1,$2,true)`, setting, value); err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, "PREPARE recovery(text,interval) AS "+recoveryCandidate(actual.sql, candidate)); err != nil {
					t.Fatal(err)
				}
				for _, pool := range []string{"amd", "arm", "default"} {
					recoveryExplain(t, tx, out, fmt.Sprintf("dense-%s-%s-%s", candidate, scope, pool), "recovery", pool, actual.args[1])
					var got bool
					if err = tx.QueryRow(ctx, fmt.Sprintf("EXECUTE recovery('%s','600 seconds')", pool), pgx.QueryExecModeSimpleProtocol).Scan(&got); err != nil || got != (pool != "arm" && scope != "t2") {
						t.Fatalf("dense recent semantics got=%v err=%v", got, err)
					}
				}
				if _, err = tx.Exec(ctx, "DEALLOCATE recovery"); err != nil {
					t.Fatal(err)
				}
				tx.Rollback(ctx)
			}
		}
		return
	}
	for _, distribution := range []string{"tenant-uniform", "platform-uniform", "platform-varied", "tenant-varied"} {
		if strings.HasPrefix(distribution, "platform") {
			ownerExec(t, s, `UPDATE hosts SET tenant_id=NULL; UPDATE pools SET tenant_id=NULL`)
		} else {
			ownerExec(t, s, `UPDATE hosts SET tenant_id='t1'; UPDATE pools SET tenant_id='t1'`)
		}
		if strings.HasSuffix(distribution, "varied") {
			ownerExec(t, s, `UPDATE placements SET ended_at=now()-interval '1 hour'-(substring(id from 2)::int * interval '1 second') WHERE ended_at IS NOT NULL`)
		}
		ownerExec(t, s, `ANALYZE hosts; ANALYZE placements; ANALYZE runs; ANALYZE pools`)
		// A real acquired application pool connection, same SQL/typed bind args,
		// default auto mode, repeated across transaction-local system scopes.
		conn, err := s.db.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12; i++ {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `SELECT set_config('lux.system','on',true)`); err != nil {
				t.Fatal(err)
			}
			var active bool
			if err = tx.QueryRow(ctx, actual.sql, "amd", actual.args[1]).Scan(&active); err != nil || active {
				t.Fatalf("actual execution active=%v err=%v", active, err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var name, types string
		var generic, custom int64
		if err = conn.QueryRow(ctx, `SELECT name,parameter_types::text,generic_plans,custom_plans FROM pg_prepared_statements WHERE statement=$1`, actual.sql).Scan(&name, &types, &generic, &custom); err != nil {
			t.Fatal(err)
		}
		t.Logf("AUTO distribution=%s mode=%v name=%s types=%s generic=%d custom=%d", distribution, conn.Conn().Config().DefaultQueryExecMode, name, types, generic, custom)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SELECT set_config('lux.system','on',true)`); err != nil {
			t.Fatal(err)
		}
		recoveryExplain(t, tx, out, fmt.Sprintf("%d-%s-auto", size, distribution), name, "amd", actual.args[1])
		tx.Rollback(ctx)
		conn.Release()
		for _, candidate := range []string{"original", "materialized", "ended-only"} {
			if os.Getenv("RECOVERY_WINNER_ONLY") == "1" && candidate != "materialized" {
				continue
			}
			for _, scope := range []string{"system", "t1", "t2"} {
				for _, mode := range []string{"force_generic_plan", "force_custom_plan"} {
					for _, missing := range []bool{false, true} {
						if missing && (distribution != "tenant-uniform" || scope != "system" || mode != "force_generic_plan") {
							continue
						}
						tx, err := owner.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if missing {
							if _, err = tx.Exec(ctx, `DROP INDEX placements_recent_ended; DROP INDEX placements_recent_created`); err != nil {
								t.Fatal(err)
							}
						}
						if _, err = tx.Exec(ctx, `SET LOCAL ROLE lux_app`); err != nil {
							t.Fatal(err)
						}
						setting, value := "lux.tenant_id", scope
						if scope == "system" {
							setting, value = "lux.system", "on"
						}
						if _, err = tx.Exec(ctx, `SELECT set_config($1,$2,true)`, setting, value); err != nil {
							t.Fatal(err)
						}
						if _, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode); err != nil {
							t.Fatal(err)
						}
						query := recoveryCandidate(actual.sql, candidate)
						if candidate != "original" && query == actual.sql {
							t.Fatal("candidate replacement failed")
						}
						if _, err = tx.Exec(ctx, "PREPARE recovery(text, interval) AS "+query); err != nil {
							t.Fatal(err)
						}
						for _, pool := range []string{"amd", "arm", "default"} {
							label := fmt.Sprintf("%d-%s-%s-%s-%s-missing%t-%s", size, distribution, candidate, scope, mode, missing, pool)
							blocks := recoveryExplain(t, tx, out, label, "recovery", pool, actual.args[1])
							if !missing && candidate != "original" && blocks > poolActivityBufferBudget {
								t.Errorf("%s: %.0f buffers exceeds unchanged budget %d", label, blocks, poolActivityBufferBudget)
							}
							if missing && pool == "amd" && blocks <= poolActivityBufferBudget {
								t.Errorf("%s: missing-index negative control not rejected: %.0f", label, blocks)
							}
							if candidate == "original" && !missing && distribution == "tenant-uniform" && scope == "system" && mode == "force_generic_plan" && pool == "amd" && blocks <= poolActivityBufferBudget {
								t.Errorf("original-query negative control not rejected: %.0f", blocks)
							}
							var active bool
							q := fmt.Sprintf("EXECUTE recovery('%s','%s')", pool, actual.args[1])
							if err = tx.QueryRow(ctx, q, pgx.QueryExecModeSimpleProtocol).Scan(&active); err != nil || active != (pool == "default" && scope != "t2") {
								t.Fatalf("%s active=%v err=%v", label, active, err)
							}
						}
						if !missing && candidate != "original" && mode == "force_generic_plan" {
							recoverySemantics(t, tx, scope)
						}
						if _, err = tx.Exec(ctx, "DEALLOCATE recovery"); err != nil {
							t.Fatal(err)
						}
						tx.Rollback(ctx)
					}
				}
			}
		}
	}
}

func recoverySemantics(t *testing.T, tx pgx.Tx, scope string) {
	t.Helper()
	if scope == "t2" {
		return
	} // Matrix already asserts invisible t1 live placement.
	ctx := context.Background()
	for _, c := range []struct {
		state, created, ended string
		spot, want            bool
	}{
		{"assigned", "now()-interval '1 day'", "NULL", false, true},
		{"starting", "now()-interval '1 day'", "NULL", false, true},
		{"running", "now()-interval '1 day'", "NULL", false, true},
		{"stopping", "now()-interval '1 day'", "NULL", false, true},
		{"exited", "now()-interval '1 day'", "now()", false, true},
		{"exited", "now()", "NULL", false, true},
		{"exited", "now()-interval '1 day'", "NULL", false, false},
		{"running", "now()", "NULL", true, false},
		{"exited", "now()", "now()", true, false},
		{"exited", "now()", "NULL", true, false},
		{"exited", "now()-interval '1 day'", "now()-interval '10 minutes'", false, false},
		{"exited", "now()-interval '1 day'", "now()-interval '10 minutes'+interval '1 microsecond'", false, true},
		{"exited", "now()-interval '10 minutes'", "NULL", false, false},
		{"exited", "now()-interval '10 minutes'+interval '1 microsecond'", "NULL", false, true},
	} {
		if _, err := tx.Exec(ctx, `UPDATE runs SET spec=jsonb_build_object('spot',$1::boolean) WHERE id='r1'`, c.spot); err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,created_at,ended_at) VALUES ('semantic','t1','r1','h1',2,$1,%s,%s)`, c.created, c.ended)
		if _, err := tx.Exec(ctx, q, c.state); err != nil {
			t.Fatal(err)
		}
		var got bool
		if err := tx.QueryRow(ctx, `EXECUTE recovery('amd','600 seconds')`, pgx.QueryExecModeSimpleProtocol).Scan(&got); err != nil || got != c.want {
			t.Fatalf("semantic scope=%s case=%+v got=%v err=%v", scope, c, got, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM placements WHERE id='semantic'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET spec='{}' WHERE id='r1'`); err != nil {
		t.Fatal(err)
	}
}

func recoveryExplain(t *testing.T, tx pgx.Tx, out, label, name, pool string, arg any) float64 {
	t.Helper()
	ctx := context.Background()
	var plan json.RawMessage
	q := fmt.Sprintf("EXPLAIN (ANALYZE, BUFFERS, SETTINGS, FORMAT JSON) EXECUTE %s('%s','%s')", name, pool, arg)
	if err := tx.QueryRow(ctx, q, pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, label+".json"), plan, 0644); err != nil {
		t.Fatal(err)
	}
	var p []map[string]any
	if err := json.Unmarshal(plan, &p); err != nil {
		t.Fatal(err)
	}
	root := p[0]["Plan"].(map[string]any)
	if _, jit := p[0]["JIT"]; jit {
		t.Errorf("%s: unexpected JIT", label)
	}
	var generic, custom int64
	var types, role, mode string
	var bypass bool
	if err := tx.QueryRow(ctx, `SELECT generic_plans,custom_plans,parameter_types::text,current_user,current_setting('plan_cache_mode'),(SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user) FROM pg_prepared_statements WHERE name=$1`, name).Scan(&generic, &custom, &types, &role, &mode, &bypass); err != nil {
		t.Fatal(err)
	}
	if role != "lux_app" || bypass {
		t.Fatalf("invalid application RLS role: %s bypass=%v", role, bypass)
	}
	t.Logf("PLAN %s buffers=%.0f execution_ms=%v generic=%d custom=%d types=%s role=%s bypass=%v mode=%s", label, root["Shared Hit Blocks"].(float64)+root["Shared Read Blocks"].(float64), p[0]["Execution Time"], generic, custom, types, role, bypass, mode)
	return root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64)
}
