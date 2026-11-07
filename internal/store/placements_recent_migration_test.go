package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

func TestPlacementsRecentMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "094_tenant_events"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// Compare every existing index and policy, including RLS enable/force flags.
	const catalog = `SELECT jsonb_build_object(
		'indexes', (SELECT jsonb_agg(indexdef ORDER BY indexname) FROM pg_indexes
			WHERE schemaname = 'public' AND indexname NOT IN ('placements_recent_ended', 'placements_recent_created')),
		'policies', (SELECT jsonb_agg(to_jsonb(p) ORDER BY tablename, policyname) FROM pg_policies p WHERE schemaname = 'public'),
		'rls', (SELECT jsonb_agg(jsonb_build_array(relname, relrowsecurity, relforcerowsecurity) ORDER BY relname)
			FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind = 'r'))::text`
	var before, after string
	if err := conn.QueryRow(ctx, catalog).Scan(&before); err != nil {
		t.Fatal(err)
	}
	done, err := store.Migrate(ctx, owner, "lux_app")
	if err != nil || !slices.Equal(done, []string{"095_placements_recent"}) {
		t.Fatalf("upgrade applied %v: %v", done, err)
	}
	if err := conn.QueryRow(ctx, catalog).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("migration changed existing indexes or RLS")
	}
	for _, c := range []struct{ name, column, predicate string }{
		{"placements_recent_ended", "ended_at", "(ended_at IS NOT NULL)"},
		{"placements_recent_created", "created_at", "(ended_at IS NULL)"},
	} {
		var column, predicate, method string
		var valid, ready, unique bool
		var keys, attrs int
		err := conn.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid, 1, true), pg_get_expr(i.indpred, i.indrelid),
			a.amname, i.indisvalid, i.indisready, i.indisunique, i.indnkeyatts, i.indnatts
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am a ON a.oid = c.relam
			WHERE i.indexrelid = $1::regclass AND i.indrelid = 'placements'::regclass`, c.name).
			Scan(&column, &predicate, &method, &valid, &ready, &unique, &keys, &attrs)
		if err != nil || column != c.column || predicate != c.predicate || method != "btree" || !valid || !ready || unique || keys != 1 || attrs != 1 {
			t.Errorf("%s: column=%s predicate=%s method=%s valid=%t ready=%t unique=%t keys=%d attrs=%d err=%v",
				c.name, column, predicate, method, valid, ready, unique, keys, attrs, err)
		}
	}
	if done, err := store.Migrate(ctx, owner, "lux_app"); err != nil || len(done) != 0 {
		t.Fatalf("second migration: %v %v", done, err)
	}
}
