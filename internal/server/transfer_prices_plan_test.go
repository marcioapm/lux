package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

// Frozen baseline. The candidate must come from refreshTransferPrices itself.
const oldTransferSelector = `SELECT DISTINCT p.provider, h.launch_template->>'region'
	FROM hosts h JOIN pools p ON p.id = h.pool_id
	WHERE h.provision_requested_at IS NOT NULL AND p.provider <> 'static' AND coalesce(h.launch_template->>'region', '') <> ''
		AND (h.terminated_at IS NULL OR EXISTS (SELECT 1 FROM cost_transfer_snapshots t WHERE t.host_id = h.id AND NOT t.finalized))
	ORDER BY 1, 2`

func TestTransferPricesSelectorWork(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	ownerExec(t, s, `INSERT INTO pools (id, tenant_id, name, provider) VALUES
		('transfer-t1', 't1', 'transfer', 'ec2'), ('transfer-t2', 't2', 'transfer', 'ec2'),
		('transfer-platform', NULL, 'transfer', 'ec2'), ('transfer-static', 't1', 'transfer-static', 'static');
		ALTER TABLE hosts SET (autovacuum_enabled = false);
		ALTER TABLE cost_transfer_snapshots SET (autovacuum_enabled = false);
		INSERT INTO hosts (id, name, tenant_id, pool_id, state, provision_requested_at, terminated_at,
			launch_template, labels, capacity, versions, caches)
		SELECT 'transfer-' || i, 'transfer-' || i,
			CASE WHEN i = 4 THEN NULL WHEN i = 3 THEN 't2' ELSE 't1' END,
			CASE i WHEN 3 THEN 'transfer-t2' WHEN 4 THEN 'transfer-platform' WHEN 7 THEN 'transfer-static' ELSE 'transfer-t1' END,
			CASE WHEN i <= 4 THEN 'ready' ELSE 'terminated' END,
			CASE WHEN i <> 10 THEN now() - interval '1 day' END,
			CASE WHEN i > 4 THEN now() END,
			CASE i WHEN 8 THEN '{}'::jsonb WHEN 9 THEN '{"region":""}'::jsonb WHEN 11 THEN '{"region":null}'::jsonb
				ELSE jsonb_build_object('region', CASE WHEN i <= 2 THEN 'live-a' WHEN i = 3 THEN 'live-b'
					WHEN i = 4 THEN 'shared' WHEN i <= 6 THEN 'pending' ELSE 'excluded' END,
					'image', 'ami-fixture', 'instanceType', 'm7i.large') END,
			jsonb_build_object('metadata', (SELECT string_agg(md5((i * 100 + k)::text), '') FROM generate_series(1, 55) k)),
			'{"cpus":2,"memory":8589934592}', '{"runner":"fixture"}', '{"images":["fixture"]}'
		FROM generate_series(1, 3400) i;
		INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		SELECT 'transfer-p' || i, 't1', 'r1', 'transfer-' || CASE WHEN i <= 14 THEN 5 + (i - 1) % 7 ELSE 12 + (i - 15) % 3389 END,
			i, 'exited' FROM generate_series(1, 28000) i;
		INSERT INTO cost_transfer_snapshots (placement_id, run_id, tenant_id, host_id, finalized, priced_to, items, hours)
		SELECT id, run_id, tenant_id, host_id, epoch > 14, now(),
			jsonb_build_array(jsonb_build_object('class', 'internet', 'tx', 123456789, 'price', '0.09')),
			jsonb_build_array(jsonb_build_object('at', '2026-11-01T00:00:00Z', 'amount', '0.01'))
		FROM placements;
		ANALYZE hosts; ANALYZE pools; ANALYZE cost_transfer_snapshots;`)
	// Cache hits keep the full periodic function isolated from provider/network work.
	execSQL(t, s, ctx, `INSERT INTO price_cache (provider, region, instance_type, os, per_hour, currency, fetched_at)
		SELECT 'ec2', r, 'internet:tx', 'TRANSFER', 0.09, 'USD', now()
		FROM unnest(ARRAY['live-a','live-b','shared','pending']) r`)
	s.cfg.Costs.PricesRefresh = time.Hour
	cfg := s.db.Pool.Config()
	rec := &feedRecorder{}
	cfg.ConnConfig.Tracer = rec
	db, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	orig := s.db
	s.db = db
	started := time.Now()
	s.refreshTransferPrices(ctx)
	t.Logf("full function (warm cache): %s", time.Since(started))
	s.db = orig
	db.Close()
	var actual string
	var refreshed []string
	for _, q := range rec.got {
		if strings.Contains(q.sql, "SELECT DISTINCT p.provider") {
			if actual != "" {
				t.Fatal("multiple region selectors")
			}
			actual = q.sql
		}
		if strings.Contains(q.sql, "FROM price_cache WHERE provider = $1") {
			refreshed = append(refreshed, fmt.Sprint(q.args[0])+"/"+fmt.Sprint(q.args[1]))
		}
	}
	if actual == "" {
		t.Fatal("refreshTransferPrices did not execute the region selector")
	}
	if want := []string{"ec2/live-a", "ec2/live-b", "ec2/pending", "ec2/shared"}; !reflect.DeepEqual(refreshed, want) {
		t.Fatalf("full function refreshed %v, want %v", refreshed, want)
	}
	t.Logf("actual executed selector: %s", actual)
	for _, scope := range []struct {
		name string
		want []string
	}{
		{"system", []string{"ec2/live-a", "ec2/live-b", "ec2/pending", "ec2/shared"}},
		{"t1", []string{"ec2/live-a", "ec2/pending", "ec2/shared"}},
		{"t2", []string{"ec2/live-b", "ec2/shared"}},
	} {
		t.Run(scope.name, func(t *testing.T) {
			err := pgx.BeginTxFunc(ctx, s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
				// Both statements see exactly the same snapshot, including under tenant RLS.
				setting, value := "lux.tenant_id", scope.name
				if scope.name == "system" {
					setting, value = "lux.system", "on"
				}
				if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
					return err
				}
				var role, jit, thresholds string
				var bypass bool
				if err := tx.QueryRow(ctx, `SELECT current_user, rolsuper OR rolbypassrls, current_setting('jit'),
					current_setting('jit_above_cost') || '/' || current_setting('jit_inline_above_cost') || '/' || current_setting('jit_optimize_above_cost')
					FROM pg_roles WHERE rolname = current_user`).Scan(&role, &bypass, &jit, &thresholds); err != nil {
					return err
				}
				if role != "lux_app" || bypass || jit != "on" || thresholds != "100000/500000/500000" {
					t.Fatalf("need app RLS and default JIT: role=%s bypass=%t jit=%s thresholds=%s", role, bypass, jit, thresholds)
				}
				for _, query := range []string{oldTransferSelector, actual} {
					rows, err := tx.Query(ctx, query)
					if err != nil {
						return err
					}
					got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
						var provider, region string
						err := r.Scan(&provider, &region)
						return provider + "/" + region, err
					})
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, scope.want) {
						t.Errorf("regions=%v want=%v SQL=%s", got, scope.want, query)
					}
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
						return err
					}
					for i, query := range []string{oldTransferSelector, actual} {
						name := fmt.Sprintf("transfer_selector_%d", i)
						if _, err := tx.Exec(ctx, "PREPARE "+name+" AS "+query); err != nil {
							return err
						}
						// Baseline cost alone suffices: do not pay its JIT compilation every test.
						options := "FORMAT JSON"
						if i == 1 {
							options = "ANALYZE, BUFFERS, FORMAT JSON"
						}
						var plan []map[string]any
						if err := tx.QueryRow(ctx, "EXPLAIN ("+options+") EXECUTE "+name).Scan(&plan); err != nil {
							return err
						}
						if _, err := tx.Exec(ctx, "DEALLOCATE "+name); err != nil {
							return err
						}
						encoded, _ := json.Marshal(plan)
						t.Logf("%s baseline=%t plan=%s", mode, i == 0, encoded)
						root := plan[0]["Plan"].(map[string]any)
						if i == 1 {
							blocks := root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64)
							if root["Total Cost"].(float64) >= 100000 || blocks > 20000 {
								t.Errorf("selector exceeds bounded cost/work: cost=%v buffers=%v", root["Total Cost"], blocks)
							}
							if _, compiled := plan[0]["JIT"]; compiled {
								t.Error("selector should not reach default JIT threshold")
							}
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
