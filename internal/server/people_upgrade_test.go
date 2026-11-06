package server

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// The upgrade from before people, as luxd finishes it at its first start:
// the default tenant's operators become its admins, every other email that
// submitted a Run there a member, nobody else anything; dude's tenant key
// works as before; operator flags follow the configuration both ways.
func TestPeopleUpgradeAtStartup(t *testing.T) {
	dudeKey, readKey := ids.Secret("lux"), ids.Secret("lux")
	s := testServerFrom(t, "", "091_pool_preemptible", func(c *pgx.Conn) {
		ctx := context.Background()
		for _, q := range []string{
			`INSERT INTO tenants (id, name) VALUES ('t_def', 'default'), ('t_other', 'other')`,
			`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
				('k_dude', 't_def', 'dude', '` + ids.Hash(dudeKey) + `', ARRAY['run', 'read']),
				('k_read', 't_def', 'dash', '` + ids.Hash(readKey) + `', ARRAY['read']),
				('k_op', NULL, 'marcioapm@gmail.com', 'h_op', ARRAY['operator']),
				('k_plat', NULL, 'operator', 'h_plat', ARRAY['operator'])`,
			`INSERT INTO runs (id, tenant_id, spec, state, submitted_by_email, submitted_by_key) VALUES
				('r1', 't_def', '{}', 'running', 'marcioapm@gmail.com', NULL),
				('r2', 't_def', '{}', 'running', 'Bob@Example.com', NULL),
				('r3', 't_def', '{}', 'running', 'bob@example.com', NULL),
				('r4', 't_other', '{}', 'running', 'carol@example.com', NULL),
				('r5', 't_def', '{}', 'running', NULL, 'k_dude')`,
		} {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	})
	ctx := context.Background()
	s.cfg.ConsoleAuth = ConsoleAuth{Mode: "cloudflare-access", CFOperators: []string{"marcioapm@gmail.com", "ops2@example.com"}, CFDefaultTenant: "default"}
	s.cfAccess = &cfAccess{}
	if err := s.initCFTenant(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a second start changes nothing
		if err := s.syncPeople(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	rows := queryAll(t, s, `SELECT m.tenant_id || ' ' || lower(u.email) || ' ' || m.role FROM memberships m JOIN users u ON u.id = m.user_id`)
	for _, r := range rows {
		got[r] = r
	}
	want := []string{"t_def bob@example.com member", "t_def marcioapm@gmail.com admin", "t_def ops2@example.com admin"}
	if gotList := slices.Sorted(mapKeys(got)); !slices.Equal(gotList, want) {
		t.Fatalf("memberships: %v, want %v", gotList, want)
	}
	if ops := queryAll(t, s, `SELECT lower(email) FROM users WHERE operator ORDER BY 1`); !slices.Equal(ops, []string{"marcioapm@gmail.com", "ops2@example.com"}) {
		t.Errorf("operators: %v", ops)
	}
	// The operator key named after Márcio is his; the other stays a platform key.
	if u := queryOne[*string](t, s, `SELECT user_id FROM api_keys WHERE id = 'k_op'`); u == nil {
		t.Error("operator key named after an operator is no one's")
	}
	if u := queryOne[*string](t, s, `SELECT user_id FROM api_keys WHERE id = 'k_plat'`); u != nil {
		t.Errorf("platform key became %s's", *u)
	}
	// dude's key: the same scopes as before, through the API.
	var who Whoami
	if code := getJSON(t, s, dudeKey, "/v1/whoami", &who); code != 200 || who.TenantID != "t_def" || !slices.Equal(who.Scopes, []string{"run", "read"}) {
		t.Errorf("dude key: %d %+v", code, who)
	}
	if code, _ := postJSON(t, s, dudeKey, "/v1/runs", minimalRun); code != http.StatusCreated {
		t.Errorf("dude key submit: %d", code)
	}
	if code, out := postJSON(t, s, readKey, "/v1/runs", minimalRun); code != http.StatusForbidden {
		t.Errorf("read key submit: %d %v", code, out)
	}
	// An email off the list stops being an operator; its membership stays.
	s.cfg.ConsoleAuth.CFOperators = []string{"marcioapm@gmail.com"}
	if err := s.syncPeople(ctx); err != nil {
		t.Fatal(err)
	}
	if ops := queryAll(t, s, `SELECT lower(email) FROM users WHERE operator`); !slices.Equal(ops, []string{"marcioapm@gmail.com"}) {
		t.Errorf("operators after removal: %v", ops)
	}
	if n := queryOne[int64](t, s, `SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id WHERE lower(u.email) = 'ops2@example.com'`); n != 1 {
		t.Errorf("removed operator's membership: %d", n)
	}
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// queryAll reads one text column of every row, as the system.
func queryAll(t *testing.T, s *Server, q string, args ...any) []string {
	t.Helper()
	var out []string
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), q, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
