package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

// A legacy account (as the upgrade made it): named after its key, which
// keeps that name; plus a second, hand-named key and a revoked one.
func legacyAccount(t *testing.T, f *peopleFix) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role, description) VALUES ('sa_ext', 't1', 'external-app', 'member', 'From API key external-app')`)
	f.keys["ext"] = ids.Secret("lux")
	f.keys["ext2"] = ids.Secret("lux_s")
	execSQL(t, f.s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, service_account_id) VALUES
		('k_ext', 't1', 'external-app', $1, '{run}', 'sa_ext'), ('k_ext2', 't1', 'rotation', $2, '{}', 'sa_ext')`, ids.Hash(f.keys["ext"]), ids.Hash(f.keys["ext2"]))
	execSQL(t, f.s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, service_account_id, revoked_at) VALUES
		('k_ext_old', 't1', 'external-app', $1, '{}', 'sa_ext', now())`, ids.Hash(ids.Secret("lux")))
}

// DELETE /v1/service-accounts/{sa}: admins and owners; only with no Runs
// (409 has_runs, the count); its keys stop at once, their rows stay revoked.
func TestServiceAccountDelete(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	ownedRuns(t, f) // r_sa is sa_bot's
	if code, _ := f.do(t, f.keys["ext"], http.MethodGet, "/v1/whoami", nil); code != 200 {
		t.Fatal("the key works before", code)
	}
	for _, key := range []string{"member", "viewer", "sa"} {
		if code, out := f.do(t, f.keys[key], http.MethodDelete, "/v1/service-accounts/external-app", nil); code != 403 {
			t.Errorf("%s deletes: %d %v", key, code, out)
		}
	}
	code, out := f.do(t, f.keys["admin"], http.MethodDelete, "/v1/service-accounts/sa-bot", nil)
	if c, msg := errCode(out); code != 409 || c != "has_runs" || !strings.Contains(msg, "owns 1 Runs") || !strings.Contains(msg, "disable") {
		t.Errorf("with Runs: %d %v", code, out)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM service_accounts WHERE id = 'sa_bot'`); n != 1 {
		t.Error("deleted despite its Runs")
	}
	execSQL(t, f.s, context.Background(), `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_t2', 't2', 'other', 'member')`)
	if code, _ := f.do(t, f.keys["admin"], http.MethodDelete, "/v1/service-accounts/sa_t2", nil); code != 404 {
		t.Errorf("another tenant's: %d", code)
	}
	if code, out := f.do(t, f.keys["owner"], http.MethodDelete, "/v1/service-accounts/external-app", nil); code != 204 {
		t.Fatalf("delete: %d %v", code, out)
	}
	for _, k := range []string{"ext", "ext2"} {
		if code, _ := f.do(t, f.keys[k], http.MethodGet, "/v1/whoami", nil); code != 401 {
			t.Errorf("%s after delete: %d, want 401", k, code)
		}
	}
	if code, _ := f.do(t, f.keys["admin"], http.MethodGet, "/v1/service-accounts/external-app", nil); code != 404 {
		t.Errorf("get after delete: %d", code)
	}
	if got := queryAll(t, f.s, `SELECT id || ':' || (revoked_at IS NOT NULL) || ':' || coalesce(service_account_id, '-') FROM api_keys WHERE id LIKE 'k_ext%' ORDER BY id`); stringify(got) != `["k_ext:true:-","k_ext2:true:-","k_ext_old:true:-"]` {
		t.Errorf("keys kept, revoked, untied: %v", got)
	}
	if got := queryAll(t, f.s, `SELECT type FROM run_events WHERE type IN ('key.revoked', 'service_account.deleted') ORDER BY id`); stringify(got) != `["key.revoked","key.revoked","service_account.deleted"]` {
		t.Errorf("events: %v", got)
	}
	// The name is free again.
	if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts", map[string]any{"name": "external-app", "role": "member"}); code != 201 {
		t.Errorf("name reused: %d %v", code, out)
	}
}

// POST /v1/service-accounts/{sa}/convert: the account's active keys become
// the member's personal keys (read-only and expiry kept), its Runs theirs,
// the account gone; one event.
func TestServiceAccountConvert(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `UPDATE api_keys SET read_only = true, expires_at = now() + interval '30 days' WHERE id = 'k_ext2'`)
	execSQL(t, f.s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, owner_sa_id) VALUES ('r_e1', 't1', '{}', 'stopped', 'sa_ext'), ('r_e2', 't1', '{}', 'running', 'sa_ext')`)
	for _, key := range []string{"member", "viewer", "sa"} {
		if code, out := f.do(t, f.keys[key], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "mia@example.com"}); code != 403 {
			t.Errorf("%s converts: %d %v", key, code, out)
		}
	}
	code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "nobody@example.com"})
	if c, _ := errCode(out); code != 404 || c != "not_a_member" {
		t.Errorf("non-member: %d %v", code, out)
	}
	execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_t2', 't2', 'other', 'member')`)
	if code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/sa_t2/convert", map[string]any{"email": "mia@example.com"}); code != 404 {
		t.Errorf("another tenant's: %d", code)
	}
	code, out = f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "mia@example.com"})
	if code != 200 || out["keys"] != float64(2) || out["runs"] != float64(2) || out["user"].(map[string]any)["id"] != "usr_member" {
		t.Fatalf("convert: %d %v", code, out)
	}
	// The keys authenticate as Mia now: her role, no service account.
	for _, k := range []string{"ext", "ext2"} {
		code, who := f.do(t, f.keys[k], http.MethodGet, "/v1/whoami", nil)
		if code != 200 || who["userId"] != "usr_member" || who["serviceAccount"] != nil || who["role"] != "member" {
			t.Errorf("%s after convert: %d %v", k, code, who)
		}
	}
	if got := queryAll(t, f.s, `SELECT id || ':' || coalesce(user_id, '-') || ':' || coalesce(service_account_id, '-') || ':' || read_only || ':' || (expires_at IS NOT NULL) || ':' || (revoked_at IS NOT NULL) FROM api_keys WHERE id LIKE 'k_ext%' ORDER BY id`); stringify(got) != `["k_ext:usr_member:-:false:false:false","k_ext2:usr_member:-:true:true:false","k_ext_old:-:-:false:false:true"]` {
		t.Errorf("keys: %v", got)
	}
	// A read-only key stays read-only.
	if code, _ := f.do(t, f.keys["ext2"], http.MethodPatch, "/v1/runs/r_e1", map[string]any{"name": "x"}); code != 403 {
		t.Errorf("read-only key writes after convert: %d", code)
	}
	code, list := f.do(t, f.keys["admin"], http.MethodGet, "/v1/runs?owner=user:usr_member", nil)
	ids := []string{}
	for _, r := range list["runs"].([]any) {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	if code != 200 || !strings.Contains(strings.Join(ids, ","), "r_e1") || !strings.Contains(strings.Join(ids, ","), "r_e2") {
		t.Errorf("Mia's Runs: %d %v", code, ids)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM service_accounts WHERE id = 'sa_ext'`); n != 0 {
		t.Error("the account is still there")
	}
	ev := queryAll(t, f.s, `SELECT (data->>'serviceAccount') || ':' || (data->>'name') || ':' || (data->>'user') || ':' || (data->>'keys') || ':' || (data->>'runs') FROM run_events WHERE type = 'service_account.converted'`)
	if stringify(ev) != `["sa_ext:external-app:usr_member:2:2"]` {
		t.Errorf("event: %v", ev)
	}
	if code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "mia@example.com"}); code != 404 {
		t.Errorf("converted twice: %d", code)
	}
}

// The escalation guard: the keys act with the member's role, so a caller
// converts only to a member at most their own role.
func TestServiceAccountConvertGuard(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "olive@example.com"})
	if code != 403 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "owner") {
		t.Errorf("admin to an owner: %d %v", code, out)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE service_account_id = 'sa_ext'`); n != 3 {
		t.Errorf("keys moved on a refusal: %d left", n)
	}
	// An admin to an admin (themselves) is fine; an owner to an owner too.
	if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "adam@example.com"}); code != 200 {
		t.Errorf("admin to admin: %d %v", code, out)
	}
	execSQL(t, f.s, context.Background(), `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_o', 't1', 'olive-laptop', 'member')`)
	if code, out := f.do(t, f.keys["owner"], http.MethodPost, "/v1/service-accounts/olive-laptop/convert", map[string]any{"email": "olive@example.com"}); code != 200 {
		t.Errorf("owner to owner: %d %v", code, out)
	}
	execSQL(t, f.s, context.Background(), `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_p', 't1', 'op-made', 'member')`)
	if code, out := f.do(t, f.keys["op"], http.MethodPost, "/v1/service-accounts/op-made/convert?tenant=t1", map[string]any{"email": "olive@example.com"}); code != 200 {
		t.Errorf("operator to owner: %d %v", code, out)
	}
}

// 409 key_limit when the member would have more than 5 active keys:
// nothing moves.
func TestServiceAccountConvertCap(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f) // 2 active keys
	for i := 0; i < 3; i++ {
		f.addKey(t, "mia"+string(rune('a'+i)), "user_id", "usr_member")
	}
	// Mia has 4 active (k_member + 3), plus an expired and a revoked one that do not count.
	execSQL(t, f.s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id, expires_at) VALUES ('k_mia_exp', 't1', 'exp', 'h1', '{}', 'usr_member', now() - interval '1 day')`)
	execSQL(t, f.s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id, revoked_at) VALUES ('k_mia_rev', 't1', 'rev', 'h2', '{}', 'usr_member', now())`)
	code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "mia@example.com"})
	if c, _ := errCode(out); code != 409 || c != "key_limit" {
		t.Fatalf("over the cap: %d %v", code, out)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE service_account_id = 'sa_ext'`); n != 3 {
		t.Errorf("keys moved: %d left", n)
	}
	if code, _ := f.do(t, f.keys["ext"], http.MethodGet, "/v1/whoami", nil); code != 200 {
		t.Error("the account's key stopped working")
	}
	// One revoked: 3 + 2 = 5 fits exactly.
	execSQL(t, f.s, context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE id = 'k_miaa'`)
	if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": "mia@example.com"}); code != 200 {
		t.Errorf("at the cap: %d %v", code, out)
	}
}

// Two converts of one account at once: one moves everything, the other
// finds no account (404) or is refused; never half of each.
func TestServiceAccountConvertConcurrent(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	execSQL(t, f.s, context.Background(), `INSERT INTO runs (id, tenant_id, spec, state, owner_sa_id) VALUES ('r_e1', 't1', '{}', 'stopped', 'sa_ext')`)
	codes := make(chan int, 2)
	for _, email := range []string{"mia@example.com", "max@example.com"} {
		go func(email string) {
			code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": email})
			codes <- code
		}(email)
	}
	a, b := <-codes, <-codes
	if !((a == 200 && (b == 404 || b == 409)) || (b == 200 && (a == 404 || a == 409))) {
		t.Fatalf("codes %d %d", a, b)
	}
	owners := queryAll(t, f.s, `SELECT DISTINCT coalesce(user_id, '-') FROM api_keys WHERE id IN ('k_ext', 'k_ext2')`)
	runOwner := queryOne[*string](t, f.s, `SELECT owner_user_id FROM runs WHERE id = 'r_e1'`)
	if len(owners) != 1 || runOwner == nil || owners[0] != *runOwner {
		t.Errorf("half state: keys %v, Run %v", owners, runOwner)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM run_events WHERE type = 'service_account.converted'`); n != 1 {
		t.Errorf("%d convert events", n)
	}
}

// PATCH /v1/service-accounts/{sa} {name}: admins and owners; validated as
// create; 409 for a taken name; the key named as the account follows it.
func TestServiceAccountRename(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	for _, key := range []string{"member", "viewer", "sa"} {
		if code, out := f.do(t, f.keys[key], http.MethodPatch, "/v1/service-accounts/external-app", map[string]any{"name": "jervasion"}); code != 403 {
			t.Errorf("%s renames: %d %v", key, code, out)
		}
	}
	for _, bad := range []string{"Jervasion", "-x", "", "a_b"} {
		code, out := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/external-app", map[string]any{"name": bad})
		if c, _ := errCode(out); code != 422 || c != "invalid_request" {
			t.Errorf("name %q: %d %v", bad, code, out)
		}
	}
	code, out := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/external-app", map[string]any{"name": "sa-bot"})
	if c, _ := errCode(out); code != 409 || c != "conflict" {
		t.Errorf("taken name: %d %v", code, out)
	}
	code, out = f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/external-app", map[string]any{"name": "jervasion"})
	if code != 200 || out["name"] != "jervasion" || out["id"] != "sa_ext" {
		t.Fatalf("rename: %d %v", code, out)
	}
	// The active key named as the account follows; the hand-named one and the revoked one keep theirs.
	if got := queryAll(t, f.s, `SELECT id || '=' || name FROM api_keys WHERE service_account_id = 'sa_ext' ORDER BY id`); stringify(got) != `["k_ext=jervasion","k_ext2=rotation","k_ext_old=external-app"]` {
		t.Errorf("keys after rename: %v", got)
	}
	ev := queryAll(t, f.s, `SELECT (data->>'from') || '>' || (data->>'to') || ':' || (data->'keysRenamed')::text FROM run_events WHERE type = 'service_account.updated'`)
	if len(ev) != 1 || ev[0] != `external-app>jervasion:["k_ext"]` {
		t.Errorf("event: %v", ev)
	}
	// The key still works, by its account's new name.
	if code, out := f.do(t, f.keys["ext"], http.MethodGet, "/v1/whoami", nil); code != 200 || out["serviceAccount"].(map[string]any)["name"] != "jervasion" {
		t.Errorf("whoami after rename: %d %v", code, out)
	}
	// Renaming to its own name is no change; another tenant's account is not found.
	if code, _ := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/jervasion", map[string]any{"name": "jervasion"}); code != 200 {
		t.Error(code)
	}
	execSQL(t, f.s, context.Background(), `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_t2', 't2', 'other', 'member')`)
	if code, _ := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/sa_t2", map[string]any{"name": "mine"}); code != 404 {
		t.Errorf("another tenant's: %d", code)
	}
}
