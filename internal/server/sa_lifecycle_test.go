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
