package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 092/093: every tenant key becomes a service account's (named after it,
// deduplicated, role from its scopes), operator keys stay no one's, and
// Runs get owners: the person by email, else the key's account, else none.
func TestPeopleUpgrade(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "091_pool_preemptible"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, created_at) VALUES
			('k_dude', 't1', 'Dude Bot', 'h1', ARRAY['run', 'read'], now() - interval '3 days'),
			('k_dup', 't1', 'dude bot', 'h2', ARRAY['admin'], now() - interval '2 days'),
			('k_read', 't1', '', 'h3', ARRAY['read'], now() - interval '1 day'),
			('k_t2', 't2', 'Dude Bot', 'h4', ARRAY['admin'], now()),
			('k_op', NULL, 'ada@example.com', 'h5', ARRAY['operator'], now())`,
		`INSERT INTO runs (id, tenant_id, spec, state, submitted_by_key, submitted_by_email) VALUES
			('r_person', 't1', '{}', 'running', NULL, 'Ada@Example.com'),
			('r_person2', 't2', '{}', 'running', NULL, 'ada@example.com'),
			('r_key', 't1', '{}', 'running', 'k_dude', NULL),
			('r_opkey', 't1', '{}', 'running', 'k_op', NULL),
			('r_old', 't1', '{}', 'running', NULL, NULL)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	done, err := store.Migrate(ctx, owner, "lux_app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(done, []string{"092_people", "093_people_backfill", "094_tenant_events"}) {
		t.Fatalf("applied %v", done)
	}
	type sa struct{ tenant, name, role string }
	accounts := map[string]sa{}
	rows, err := conn.Query(ctx, `SELECT k.id, s.tenant_id, s.name, s.role, s.owner_user_id IS NULL, k.key_hash, k.scopes
		FROM api_keys k JOIN service_accounts s ON s.id = k.service_account_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, hash string
		var a sa
		var ownerless bool
		var scopes []string
		if err := rows.Scan(&id, &a.tenant, &a.name, &a.role, &ownerless, &hash, &scopes); err != nil {
			t.Fatal(err)
		}
		if !ownerless {
			t.Errorf("%s: account has an owner", id)
		}
		accounts[id] = a
	}
	rows.Close()
	want := map[string]sa{
		"k_dude": {"t1", "dude-bot", "member"},
		"k_dup":  {"t1", "dude-bot-2", "admin"},
		"k_read": {"t1", "key", "viewer"},
		"k_t2":   {"t2", "dude-bot", "admin"},
	}
	if len(accounts) != len(want) {
		t.Fatalf("accounts: %+v", accounts)
	}
	for id, w := range want {
		if accounts[id] != w {
			t.Errorf("%s: %+v, want %+v", id, accounts[id], w)
		}
	}
	// The key itself is untouched: its hash and scopes, so it authenticates
	// with what it had.
	var hash string
	var scopes []string
	if err := conn.QueryRow(ctx, `SELECT key_hash, scopes FROM api_keys WHERE id = 'k_dude'`).Scan(&hash, &scopes); err != nil ||
		hash != "h1" || !slices.Equal(scopes, []string{"run", "read"}) {
		t.Errorf("k_dude: %s %v %v", hash, scopes, err)
	}
	var opSA, opUser *string
	if err := conn.QueryRow(ctx, `SELECT service_account_id, user_id FROM api_keys WHERE id = 'k_op'`).Scan(&opSA, &opUser); err != nil || opSA != nil || opUser != nil {
		t.Errorf("operator key: sa %v user %v %v", opSA, opUser, err)
	}
	var users int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 1 {
		t.Errorf("users: %d %v", users, err)
	}
	owners := map[string][2]*string{}
	rows, err = conn.Query(ctx, `SELECT r.id, lower(u.email), s.name FROM runs r LEFT JOIN users u ON u.id = r.owner_user_id LEFT JOIN service_accounts s ON s.id = r.owner_sa_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var o [2]*string
		if err := rows.Scan(&id, &o[0], &o[1]); err != nil {
			t.Fatal(err)
		}
		owners[id] = o
	}
	rows.Close()
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for id, w := range map[string][2]string{
		"r_person": {"ada@example.com", ""}, "r_person2": {"ada@example.com", ""},
		"r_key": {"", "dude-bot"}, "r_opkey": {"", ""}, "r_old": {"", ""},
	} {
		if got := [2]string{str(owners[id][0]), str(owners[id][1])}; got != w {
			t.Errorf("%s owner: %v, want %v", id, got, w)
		}
	}
	// A tenant key made the old way after the upgrade gets its own account.
	if _, err := conn.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k_new', 't1', 'dude bot', 'h6', ARRAY['read'])`); err != nil {
		t.Fatal(err)
	}
	var name, role string
	if err := conn.QueryRow(ctx, `SELECT s.name, s.role FROM api_keys k JOIN service_accounts s ON s.id = k.service_account_id WHERE k.id = 'k_new'`).Scan(&name, &role); err != nil ||
		name != "dude-bot-3" || role != "viewer" {
		t.Errorf("new legacy key: %s %s %v", name, role, err)
	}
}
