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
			path := "/v1/costs?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(from.Add(time.Duration(window)*time.Hour).Format(time.RFC3339)) + "&group=family&interval=" + interval
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
			totals, tq, _ := monthlyCapture(t, s, keys["op"], strings.Replace(path, "&interval="+interval, "", 1))
			series, sq, _ := monthlyCapture(t, s, keys["op"], path+"&runs=true")
			if !reflect.DeepEqual(got.Totals, totals.Totals) || !reflect.DeepEqual(got.Series, series.Series) {
				t.Fatal("production rows differ from unchanged totals/series paths")
			}
			qs = []feedStatement{tq[0], sq[1]}
			for _, memory := range []string{"4MB", "16MB"} {
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for attempt := 0; attempt < 3; attempt++ {
						var baseline [][]any
						for i, q := range append(slices.Clone(qs), candidate) {
							rows, plan := monthlyExplain(t, s, q, mode, memory)
							if i < 2 {
								baseline = append(baseline, rows...)
							} else {
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
	path := "/v1/costs?from=" + url.QueryEscape(t0.Add(-30*24*time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&group=family"
	_, baseline, _ := monthlyCapture(t, s, keys["op"], path)
	_, qs, _ := monthlyCapture(t, s, keys["op"], path+"&interval=day")
	if len(qs) != 1 {
		t.Fatalf("monthly heavy SQL volume=%d, want 1", len(qs))
	}
	_, before := monthlyExplain(t, s, baseline[0], "force_custom_plan", "4MB")
	_, after := monthlyExplain(t, s, qs[0], "force_custom_plan", "4MB")
	blocks := func(p map[string]any) float64 {
		r := p["Plan"].(map[string]any)
		return r["Shared Hit Blocks"].(float64) + r["Shared Read Blocks"].(float64)
	}
	t.Logf("monthly buffers totals=%v combined=%v heavy=%d", blocks(before), blocks(after), len(qs))
	if blocks(after) > blocks(before)*1.25 {
		t.Fatal("combined aggregation repeats scoped buffer work")
	}
}

func TestCostMonthlyFamilyEquivalence(t *testing.T) {
	s, keys := costReadFixture(t)
	for _, key := range []string{"t1", "t2", "op"} {
		for _, filters := range []string{"", "&family=compute", "&nofamily=compute", "&label=team=alpha", "&nolabel=team", "&family=missing"} {
			for _, interval := range []string{"hour", "day"} {
				var got, totals, series CostSummaryBody
				path := costPath("&group=family" + filters)
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

func TestCostMonthlyFamilyRowLimit(t *testing.T) {
	s, keys := costFixture(t)
	// One total per family plus one per hour: 125 * (1 + 79) = 10000.
	ownerExec(t, s, `INSERT INTO cost_hourly(hour,tenant_id,run_id,source,family,currency,amount,allocated,unallocated)
		SELECT $1::timestamptz + h * interval '1 hour','t1','r1','limit','family-' || f,'USD',1,0,0
		FROM generate_series(0,78) h, generate_series(1,125) f`, t0)
	path := "/v1/costs?from=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(80*time.Hour).Format(time.RFC3339)) + "&group=family&interval=hour"
	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], path, &got); code != 200 || len(got.Totals)+len(got.Series) != costMaxRows {
		t.Fatalf("exact limit: HTTP=%d rows=%d", code, len(got.Totals)+len(got.Series))
	}
	ownerExec(t, s, `INSERT INTO cost_hourly(hour,tenant_id,run_id,source,family,currency,amount,allocated,unallocated)
		VALUES($1,'t1','r1','limit','family-1','USD',1,0,0)`, t0.Add(79*time.Hour))
	if code := getJSON(t, s, keys["t1"], path, &got); code != 413 {
		t.Fatalf("over limit: HTTP=%d", code)
	}
}
