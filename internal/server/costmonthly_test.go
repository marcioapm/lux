package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

type monthlyRecorder struct {
	mu      sync.Mutex
	queries []feedStatement
}

func (r *monthlyRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(d.SQL), "WITH ") {
		r.mu.Lock()
		r.queries = append(r.queries, feedStatement{sql: d.SQL, args: slices.Clone(d.Args)})
		r.mu.Unlock()
	}
	return ctx
}

func (*monthlyRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Frozen from the two statements issued by the HTTP handler at ed558707,
// captured before changing costread.go. Do not build the oracle from that file.
const monthlyOldBase = `WITH scoped AS (
	SELECT c.hour, c.currency, c.amount, c.tenant_id, c.family, c.run_id,
		coalesce(cp.name, '(none)') AS pool, coalesce(c.host_id, '(none)') AS host, r.labels,
		coalesce(r.submitted_by_key, 'email:' || r.submitted_by_email, '(none)') AS submitter,
		coalesce('user:' || r.owner_user_id, 'sa:' || r.owner_sa_id, '(none)') AS member,
		coalesce('user:' || r.owner_user_id, 'user:' || sa.owner_user_id, 'sa:' || r.owner_sa_id, '(none)') AS owner
	FROM cost_hourly c JOIN runs r ON r.id = c.run_id LEFT JOIN pools cp ON cp.id = c.pool_id
	LEFT JOIN service_accounts sa ON sa.id = r.owner_sa_id
	WHERE c.run_id IS NOT NULL AND c.hour >= $1 AND c.hour < $2
		AND (cardinality($3::text[]) = 0 OR c.family = ANY($3::text[]))
		AND ($4::jsonb = '{}' OR NOT EXISTS (SELECT 1 FROM jsonb_each($4::jsonb) f
			WHERE (r.labels->>f.key) IN (SELECT jsonb_array_elements_text(f.value)) IS NOT TRUE))
		AND (cardinality($5::text[]) = 0 OR NOT EXISTS (SELECT 1 FROM unnest($5::text[]) k WHERE r.labels ? k))
), dimensions AS (
	SELECT hour, currency, amount, run_id, ($10 = 'all' OR (family = ANY($13::text[])) = ($10 = 'compute')) AS ranks,
		CASE $6::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter WHEN 'member' THEN member WHEN 'owner' THEN owner
			WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$8, '(none)') END AS g1,
		CASE $7::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter WHEN 'member' THEN member WHEN 'owner' THEN owner
			WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$9, '(none)') END AS g2
	FROM scoped WHERE family <> ALL($12::text[])
	AND ($14::boolean OR submitter IN (SELECT k.id FROM api_keys k LEFT JOIN service_accounts sa ON sa.id=k.service_account_id
		WHERE (k.user_id=nullif($15,'')) OR k.service_account_id=nullif($16,'') OR sa.owner_user_id=nullif($15,''))
		OR submitter = 'email:' || nullif($17,''))
), shaped AS (SELECT *, false AS other FROM dimensions WHERE $11::int = 0), vals AS (
	SELECT g1, currency, other, count(*)::int AS runs, NULL::int AS nvals
	FROM (SELECT DISTINCT g1, currency, other, run_id FROM shaped) d GROUP BY 1, 2, 3
) `

func monthlyOldStatements(args []any, interval string) []feedStatement {
	const key = `(currency || E'\x1f' || other::text || coalesce(E'\x1f' || g1, ''))`
	bucket := `date_trunc('day', hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`
	if interval == "hour" {
		bucket = `date_trunc('hour', hour)`
	}
	return []feedStatement{
		{sql: monthlyOldBase + `, valmap AS MATERIALIZED (SELECT jsonb_object_agg(` + key + `, jsonb_build_array(runs, nvals)) AS m FROM vals)
		SELECT at, g1, g2, currency, other, amount,
		((SELECT m FROM valmap) -> ` + key + ` ->> 0)::int, ((SELECT m FROM valmap) -> ` + key + ` ->> 1)::int
		FROM (SELECT NULL::timestamptz AS at, g1, g2, currency, other, trim_scale(sum(amount))::text AS amount FROM shaped GROUP BY 1, 2, 3, 4, 5) b ORDER BY 1 NULLS FIRST, 2, 3, 4, 5`, args: args},
		{sql: monthlyOldBase + `SELECT *, NULL::int, NULL::int FROM (SELECT ` + bucket + ` AS at, g1, g2, currency, other, trim_scale(sum(amount))::text AS amount FROM shaped GROUP BY 1, 2, 3, 4, 5) b ORDER BY 1 NULLS FIRST, 2, 3, 4, 5`, args: args},
	}
}

func monthlyFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ownerExec(t, s, `INSERT INTO runs (id, tenant_id, spec, state, labels, owner_sa_id)
		SELECT 'monthly-' || i, CASE WHEN i % 10 = 0 THEN 't2' ELSE 't1' END,
			jsonb_build_object('metadata', (SELECT string_agg(md5((i * 100 + k)::text), '') FROM generate_series(1, 45) k)),
			'exited', jsonb_build_object('app', 'app-' || i % 40, 'team', 'team-' || i % 7),
			(SELECT service_account_id FROM api_keys WHERE id = CASE WHEN i % 10 = 0 THEN 'k2' ELSE 'k1' END)
		FROM generate_series(1, 27000) i`)
	ownerExec(t, s, `INSERT INTO cost_hourly (hour, tenant_id, run_id, pool_id, source, family, currency, amount, allocated, unallocated)
		SELECT $1::timestamptz + ((i * 13 + h) % 720) * interval '1 hour',
			CASE WHEN i % 10 = 0 THEN 't2' ELSE 't1' END, 'monthly-' || i,
			(SELECT id FROM pools WHERE name = CASE WHEN i % 2 = 0 THEN 'blue' ELSE 'red' END LIMIT 1),
			'monthly', (ARRAY['compute','block-storage','public-ip','data-transfer','ai','api','storage','other'])[h + 1],
			CASE WHEN i % 17 = 0 AND h = 0 THEN 'EUR' ELSE 'USD' END, (i % 97 + 1)::numeric / 10000, 0, 0
		FROM generate_series(1, 27000) i, generate_series(0, 7) h`, t0.Add(-30*24*time.Hour))
	ownerExec(t, s, `ANALYZE runs; ANALYZE cost_hourly; ANALYZE pools; ANALYZE service_accounts`)
	return s, keys
}

func monthlyCapture(t *testing.T, s *Server, key, path string) (CostSummaryBody, []feedStatement, time.Duration) {
	t.Helper()
	cfg := s.db.Pool.Config()
	r := &monthlyRecorder{}
	cfg.ConnConfig.Tracer = r
	db, err := store.OpenConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	orig := s.db
	s.db = db
	defer func() { s.db = orig; db.Close() }()
	var body CostSummaryBody
	start := time.Now()
	if code := getJSON(t, s, key, path, &body); code != 200 {
		t.Fatalf("HTTP %d: %s", code, path)
	}
	return body, r.queries, time.Since(start)
}

func monthlyExplain(t *testing.T, s *Server, q feedStatement, mode, memory string) ([][]any, map[string]any) {
	t.Helper()
	var result [][]any
	var plan []map[string]any
	err := s.costTx(context.Background(), Principal{Operator: true}, func(tx pgx.Tx) error {
		ctx := context.Background()
		var role string
		var bypass bool
		if err := tx.QueryRow(ctx, `SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&role, &bypass); err != nil {
			return err
		}
		if role != "lux_app" || bypass {
			t.Fatalf("role=%s bypass=%t", role, bypass)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL work_mem='"+memory+"'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `PREPARE monthly(timestamptz,timestamptz,text[],jsonb,text[],text,text,text,text,text,int,text[],text[],boolean,text,text,text) AS `+q.sql); err != nil {
			return err
		}
		defer tx.Exec(ctx, "DEALLOCATE monthly")
		params := make([]string, len(q.args))
		for i := range params {
			params[i] = fmt.Sprintf("$%d", i+1)
		}
		execute := "EXECUTE monthly(" + strings.Join(params, ",") + ")"
		args := append([]any{pgx.QueryExecModeSimpleProtocol}, q.args...)
		encoded, err := json.Marshal(q.args[3])
		if err != nil {
			return err
		}
		args[4] = string(encoded)
		rows, err := tx.Query(ctx, execute, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			v, err := rows.Values()
			if err != nil {
				rows.Close()
				return err
			}
			result = append(result, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+execute, args...).Scan(&plan)
	})
	if err != nil {
		t.Fatal(err)
	}
	return result, plan[0]
}

func TestCostMonthlyExperiment(t *testing.T) {
	if os.Getenv("LUX_MONTHLY_EXPERIMENT") == "" {
		t.Skip("opt-in production-sized benchmark")
	}
	s, keys := monthlyFixture(t)
	from := t0.Add(-30 * 24 * time.Hour)
	for _, window := range []int{24, 720} {
		for _, interval := range []string{"hour", "day"} {
			path := "/v1/costs?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(from.Add(time.Duration(window)*time.Hour).Format(time.RFC3339)) + "&group=family&runs=true&interval=" + interval
			if os.Getenv("LUX_MONTHLY_RESTART") == "lux-monthly-pg" {
				if output, err := exec.Command("podman", "restart", "lux-monthly-pg").CombinedOutput(); err != nil {
					t.Fatalf("local restart: %s: %v", output, err)
				}
				for i := 0; i < 100; i++ {
					if exec.Command("podman", "exec", "lux-monthly-pg", "pg_isready", "-U", "lux").Run() == nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			got, qs, elapsed := monthlyCapture(t, s, keys["op"], path)
			t.Logf("window=%d interval=%s first HTTP=%s heavy=%d restart=%t", window, interval, elapsed, len(qs), os.Getenv("LUX_MONTHLY_RESTART") != "")
			var timings []time.Duration
			for attempt := 0; attempt < 3; attempt++ {
				_, _, elapsed := monthlyCapture(t, s, keys["op"], path)
				timings = append(timings, elapsed)
			}
			slices.Sort(timings)
			t.Logf("window=%d interval=%s warm-data fresh-pool HTTP median=%s attempts=%v", window, interval, timings[1], timings)
			if len(qs) != 1 {
				t.Fatalf("single pass needs 1 captured statement, got %d", len(qs))
			}
			candidate := qs[0]
			_ = got
			qs = monthlyOldStatements(candidate.args, interval)
			for i, q := range append(slices.Clone(qs), candidate) {
				t.Logf("CAPTURE window=%d interval=%s query=%d SQL:\n%s\nTYPED BINDS: %#v", window, interval, i, q.sql, q.args)
			}
			for _, memory := range []string{"4MB"} {
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for attempt := 0; attempt < 3; attempt++ {
						var baseline [][]any
						for i, q := range append(slices.Clone(qs), candidate) {
							rows, plan := monthlyExplain(t, s, q, mode, memory)
							if i < 2 {
								baseline = append(baseline, rows...)
							} else {
								if len(rows) != len(baseline) {
									t.Fatalf("candidate row count=%d, baseline=%d", len(rows), len(baseline))
								}
								for j, row := range rows {
									wantTotal := int32(0)
									if baseline[j][0] == nil {
										wantTotal = 1
									}
									if row[8] != wantTotal {
										t.Fatalf("grouping discriminator %v want %v", row[8], wantTotal)
									}
									rows[j] = row[:8]
								}
								if !reflect.DeepEqual(rows, baseline) {
									t.Fatal("candidate differs from full ordered baseline rows")
								}
							}
							root := plan["Plan"].(map[string]any)
							jit, _ := json.Marshal(plan["JIT"])
							t.Logf("window=%d interval=%s mem=%s mode=%s attempt=%d query=%d ms=%v buffers=%v+%v temp=%v/%v JIT=%s", window, interval, memory, mode, attempt, i, plan["Execution Time"], root["Shared Hit Blocks"], root["Shared Read Blocks"], root["Temp Read Blocks"], root["Temp Written Blocks"], jit)
						}
					}
				}
			}
		}
	}
}

// Query volume and buffer work, rather than host-dependent wall time, guard
// against reintroducing the second scoped join over a monthly window.
func TestCostMonthlyWork(t *testing.T) {
	s, keys := monthlyFixture(t)
	path := "/v1/costs?from=" + url.QueryEscape(t0.Add(-30*24*time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&group=family&runs=true"
	_, baseline, _ := monthlyCapture(t, s, keys["op"], path)
	_, qs, _ := monthlyCapture(t, s, keys["op"], path+"&interval=day")
	if len(qs) != 1 {
		t.Fatalf("monthly heavy SQL volume=%d, want 1", len(qs))
	}
	_, before := monthlyExplain(t, s, baseline[0], "force_custom_plan", "4MB")
	_, after := monthlyExplain(t, s, qs[0], "force_custom_plan", "4MB")
	work := func(p map[string]any) (float64, float64) {
		r := p["Plan"].(map[string]any)
		return r["Shared Hit Blocks"].(float64) + r["Shared Read Blocks"].(float64), r["Temp Read Blocks"].(float64) + r["Temp Written Blocks"].(float64)
	}
	beforeShared, beforeTemp := work(before)
	afterShared, afterTemp := work(after)
	t.Logf("monthly work totals=%v/%v combined=%v/%v heavy=%d", beforeShared, beforeTemp, afterShared, afterTemp, len(qs))
	if afterShared > beforeShared*1.25 || afterTemp > beforeTemp*1.25 {
		t.Fatal("combined aggregation repeats scoped buffer work")
	}
}

func TestCostMonthlyFamilyEquivalence(t *testing.T) {
	s, keys := costReadFixture(t)
	// The same Run spans hours and UTC days, including a zero cost row.
	ownerExec(t, s, `INSERT INTO cost_hourly(hour,tenant_id,run_id,source,family,currency,amount,allocated,unallocated)
		VALUES ($1,'t1','r1','overlap','compute','USD',0,0,0),
		($1,'t1','r1','overlap','(none)','EUR',1.230000000,0,0)`, t0.Add(25*time.Hour))
	for _, key := range []string{"t1", "t2", "op"} {
		for _, filters := range []string{"", "&family=compute", "&nofamily=compute", "&label=team=alpha", "&nolabel=team", "&label=team=missing", "&family=missing", "&tenant=t1"} {
			for _, interval := range []string{"hour", "day"} {
				for _, runs := range []bool{false, true} {
					var got, totals, series CostSummaryBody
					path := "/v1/costs?from=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(48*time.Hour).Format(time.RFC3339)) + "&group=family" + filters
					if runs {
						path += "&runs=true"
					}
					if runs {
						got, qs, _ := monthlyCapture(t, s, keys[key], path+"&interval="+interval)
						if len(qs) != 1 {
							t.Fatalf("runs=true heavy statements=%d, want 1", len(qs))
						}
						var want CostSummaryBody
						if code := getJSON(t, s, keys[key], path, &want); code != 200 {
							t.Fatal(code)
						}
						p := Principal{Operator: key == "op"}
						if key != "op" {
							p.TenantID = key
						}
						if key == "op" && strings.Contains(filters, "tenant=t1") {
							p.TenantID = "t1"
						}
						old := monthlyOldStatements(qs[0].args, interval)
						for i, q := range old {
							rows := []CostSummaryRow{}
							err := s.costTx(context.Background(), p, func(tx pgx.Tx) error {
								r, err := tx.Query(context.Background(), q.sql, q.args...)
								if err != nil {
									return err
								}
								defer r.Close()
								for r.Next() {
									var row CostSummaryRow
									var g1, g2 *string
									var nvals *int
									if err := r.Scan(&row.At, &g1, &g2, &row.Currency, &row.Other, &row.Amount, &row.Runs, &nvals); err != nil {
										return err
									}
									if row.At != nil {
										at := row.At.UTC()
										row.At = &at
									}
									row.Group = map[string]string{"family": *g1}
									rows = append(rows, row)
									if i == 0 && (row.Runs == nil || *row.Runs < 1) {
										t.Fatalf("missing total count: %+v", row)
									}
									if i == 1 && row.Runs != nil {
										t.Fatal("series count must be absent")
									}
								}
								return r.Err()
							})
							if err != nil {
								t.Fatal(err)
							}
							if i == 0 {
								want.Totals = rows
							} else if len(rows) > 0 {
								want.Series = rows
							}
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("runs=true %s %s %s: full response differs\ngot=%+v\nwant=%+v", key, filters, interval, got, want)
						}
						continue
					}
					for _, request := range []struct {
						path string
						out  *CostSummaryBody
					}{
						{path + "&interval=" + interval, &got}, {path, &totals}, {path + "&interval=" + interval + "&runs=true", &series},
					} {
						if code := getJSON(t, s, keys[key], request.path, request.out); code != 200 {
							t.Fatalf("%s: %d", request.path, code)
						}
					}
					if !reflect.DeepEqual(got.Totals, totals.Totals) || !reflect.DeepEqual(got.Series, series.Series) || !reflect.DeepEqual(got.Unallocated, totals.Unallocated) || !reflect.DeepEqual(got.Families, totals.Families) {
						t.Fatalf("%s %s %s: combined result differs", key, filters, interval)
					}
				}
			}
		}
	}
}

func TestCostMonthlyFamilyRowLimit(t *testing.T) {
	s, keys := costFixture(t)
	// One total per family plus one per hour: 125 * (1 + 79) = 10000.
	ownerExec(t, s, `INSERT INTO cost_hourly(hour,tenant_id,run_id,source,family,currency,amount,allocated,unallocated)
		SELECT $1::timestamptz + h * interval '1 hour','t1','r1','limit','family-' || f,'USD',1,0,0
		FROM generate_series(0,78) h, generate_series(1,125) f`, t0)
	path := "/v1/costs?from=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(80*time.Hour).Format(time.RFC3339)) + "&group=family&interval=hour"
	var got CostSummaryBody
	for _, suffix := range []string{"", "&runs=true"} {
		got = CostSummaryBody{}
		if code := getJSON(t, s, keys["t1"], path+suffix, &got); code != 200 || len(got.Totals)+len(got.Series) != costMaxRows {
			t.Fatalf("exact limit: HTTP=%d rows=%d", code, len(got.Totals)+len(got.Series))
		}
	}
	ownerExec(t, s, `INSERT INTO cost_hourly(hour,tenant_id,run_id,source,family,currency,amount,allocated,unallocated)
		VALUES($1,'t1','r1','limit','family-126','USD',1,0,0)`, t0.Add(79*time.Hour))
	for _, suffix := range []string{"", "&runs=true"} {
		if code := getJSON(t, s, keys["t1"], path+suffix, &got); code != 413 {
			t.Fatalf("over limit: HTTP=%d", code)
		}
	}
}
