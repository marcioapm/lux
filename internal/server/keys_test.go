package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

// Personal keys: made, shown once, prefixed, listed without the secret,
// renamed, revoked; at most 5 active per person per tenant, even when made
// all at once.
func TestPersonalKeys(t *testing.T) {
	f := peopleFixture(t)
	key := f.keys["viewer"]
	code, out := f.do(t, key, http.MethodPost, "/v1/me/keys", map[string]any{"name": "laptop", "readOnly": true})
	if code != http.StatusCreated || !strings.HasPrefix(out["apiKey"].(string), "lux_u_") || !strings.HasPrefix(out["apiKey"].(string), out["prefix"].(string)) {
		t.Fatalf("create: %d %v", code, out)
	}
	secret, id := out["apiKey"].(string), out["id"].(string)
	var who Whoami
	if code := getJSON(t, f.s, secret, "/v1/whoami", &who); code != 200 || who.UserID != "usr_viewer" || !who.ReadOnly {
		t.Fatalf("new key: %d %+v", code, who)
	}
	code, out = f.do(t, key, http.MethodGet, "/v1/me/keys", nil)
	keys := out["keys"].([]any)
	if code != 200 || len(keys) != 2 || strings.Contains(stringify(out), secret) {
		t.Fatalf("list: %d %v", code, out)
	}
	if code, out := f.do(t, key, http.MethodPatch, "/v1/me/keys/"+id, map[string]any{"name": "desk"}); code != 200 || out["name"] != "desk" {
		t.Fatalf("rename: %d %v", code, out)
	}
	// Another person's key is not found among mine.
	if code, _ := f.do(t, f.keys["admin"], http.MethodDelete, "/v1/me/keys/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("someone else revokes: %d", code)
	}
	if code, _ := f.do(t, key, http.MethodDelete, "/v1/me/keys/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code := getJSON(t, f.s, secret, "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", code)
	}
	// The cap: the viewer has 1 key; 10 at once make 4 more, the rest 409.
	created, refused := concurrently(10, func() (int, map[string]any) {
		return f.do(t, key, http.MethodPost, "/v1/me/keys", map[string]any{"name": "k"})
	})
	if created != 4 || refused != 6 {
		t.Fatalf("concurrent creates: %d created, %d key_limit", created, refused)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE user_id = 'usr_viewer' AND revoked_at IS NULL`); n != 5 {
		t.Fatalf("active keys: %d", n)
	}
	// A service account has no "me".
	if code, _ := f.do(t, f.keys["sa"], http.MethodGet, "/v1/me/keys", nil); code != http.StatusNotFound {
		t.Fatalf("account's me/keys: %d", code)
	}
}

func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// concurrently runs fn n times at once: how many made something (201) and
// how many were refused 409 key_limit. Anything else fails.
func concurrently(n int, fn func() (int, map[string]any)) (created, refused int) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, out := fn()
			c, _ := errCode(out)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case code == http.StatusCreated:
				created++
			case code == http.StatusConflict && c == "key_limit":
				refused++
			}
		}()
	}
	close(start)
	wg.Wait()
	return created, refused
}

// Service accounts: admins make and change them; their owner (a member)
// rotates their keys but cannot change them; anyone else sees no keys.
// At most 2 active keys, even made at once.
func TestServiceAccounts(t *testing.T) {
	f := peopleFixture(t)
	body := map[string]any{"name": "ci-runner", "role": "member", "ownerEmail": "max@example.com", "description": "CI"}
	if code, _ := f.do(t, f.keys["member"], http.MethodPost, "/v1/service-accounts", body); code != http.StatusForbidden {
		t.Fatalf("member creates: %d", code)
	}
	code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts", body)
	if code != http.StatusCreated || out["role"] != "member" || out["owner"].(map[string]any)["email"] != "max@example.com" {
		t.Fatalf("admin creates: %d %v", code, out)
	}
	for _, bad := range []map[string]any{
		{"name": "ci-runner", "role": "member"},            // taken
		{"name": "CI_Runner", "role": "member"},            // not a DNS label
		{"name": "boss", "role": "owner"},                  // never owner
		{"name": "x", "role": "member", "ownerEmail": "z"}, // not a member
	} {
		if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts", bad); code < 400 {
			t.Errorf("%v: %d %v", bad, code, out)
		}
	}
	// Every role sees the account.
	if code, out := f.do(t, f.keys["viewer"], http.MethodGet, "/v1/service-accounts/ci-runner", nil); code != 200 || out["activeKeys"] != float64(0) {
		t.Fatalf("viewer gets: %d %v", code, out)
	}
	// The owner rotates keys; may not change the role.
	if code, _ := f.do(t, f.keys["member2"], http.MethodPatch, "/v1/service-accounts/ci-runner", map[string]any{"role": "admin"}); code != http.StatusForbidden {
		t.Fatalf("owner changes role: %d", code)
	}
	created, refused := concurrently(6, func() (int, map[string]any) {
		return f.do(t, f.keys["member2"], http.MethodPost, "/v1/service-accounts/ci-runner/keys", map[string]any{"name": "k"})
	})
	if created != 2 || refused != 4 {
		t.Fatalf("concurrent account keys: %d created, %d key_limit", created, refused)
	}
	// Others: no keys to see.
	for _, k := range []string{"member", "viewer"} {
		if code, _ := f.do(t, f.keys[k], http.MethodGet, "/v1/service-accounts/ci-runner/keys", nil); code != http.StatusNotFound {
			t.Errorf("%s lists the account's keys: %d", k, code)
		}
	}
	code, out = f.do(t, f.keys["admin"], http.MethodGet, "/v1/service-accounts/ci-runner/keys", nil)
	if code != 200 || len(out["keys"].([]any)) != 2 {
		t.Fatalf("admin lists keys: %d %v", code, out)
	}
	first := out["keys"].([]any)[0].(map[string]any)
	if code, _ := f.do(t, f.keys["member2"], http.MethodDelete, "/v1/service-accounts/ci-runner/keys/"+first["prefix"].(string), nil); code != http.StatusNoContent {
		t.Fatalf("owner revokes by prefix: %d", code)
	}
	code, out = f.do(t, f.keys["member2"], http.MethodPost, "/v1/service-accounts/ci-runner/keys", map[string]any{"name": "k3"})
	if code != http.StatusCreated || !strings.HasPrefix(out["apiKey"].(string), "lux_s_") {
		t.Fatalf("rotate: %d %v", code, out)
	}
	saKey := out["apiKey"].(string)
	// Its key submits a Run the account owns; the owner acts on it.
	code, run := postJSON(t, f.s, saKey, "/v1/runs", minimalRun)
	if code != http.StatusCreated || run["owner"].(map[string]any)["name"] != "ci-runner" {
		t.Fatalf("account submits: %d %v", code, run)
	}
	runID := run["id"].(string)
	if code, out := rename(t, f, f.keys["member2"], runID); code != 200 {
		t.Fatalf("account's owner renames its Run: %d %v", code, out)
	}
	if code, _ := rename(t, f, f.keys["member"], runID); code != http.StatusForbidden {
		t.Fatalf("another member renames it: %d", code)
	}
	// Disabled: its keys stop.
	if code, out := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/ci-runner", map[string]any{"disabled": true}); code != 200 || out["disabledAt"] == nil {
		t.Fatalf("disable: %d %v", code, out)
	}
	var who Whoami
	if code := getJSON(t, f.s, saKey, "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Fatalf("disabled account's key: %d", code)
	}
	types := queryAll(t, f.s, `SELECT type FROM run_events WHERE tenant_id = 't1' AND subject IS NOT NULL ORDER BY id`)
	for _, w := range []string{"service_account.created", "key.created", "key.revoked", "service_account.disabled"} {
		if !contains(types, w) {
			t.Errorf("no %s event in %v", w, types)
		}
	}
}

// A service account's owner, by email: set, kept when the patch leaves
// ownerEmail out, and cleared by ownerEmail "" (the console's "No owner").
func TestServiceAccountOwnerClears(t *testing.T) {
	f := peopleFixture(t)
	owner := func(out map[string]any) any {
		if o, ok := out["owner"].(map[string]any); ok {
			return o["email"]
		}
		return nil
	}
	code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts", map[string]any{"name": "nightly", "role": "member", "ownerEmail": "mia@example.com"})
	if code != http.StatusCreated || owner(out) != "mia@example.com" {
		t.Fatalf("create with owner: %d %v", code, out)
	}
	code, out = f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/nightly", map[string]any{"description": "d"})
	if code != 200 || owner(out) != "mia@example.com" {
		t.Fatalf("patch without ownerEmail keeps the owner: %d %v", code, out)
	}
	code, out = f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/nightly", map[string]any{"ownerEmail": "MAX@example.com"})
	if code != 200 || owner(out) != "max@example.com" {
		t.Fatalf("reassign (any case): %d %v", code, out)
	}
	code, out = f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/nightly", map[string]any{"ownerEmail": ""})
	if code != 200 || out["owner"] != nil {
		t.Fatalf(`ownerEmail "" clears: %d %v`, code, out)
	}
	if got := queryAll(t, f.s, `SELECT coalesce(owner_user_id, '-') FROM service_accounts WHERE name = 'nightly'`); len(got) != 1 || got[0] != "-" {
		t.Fatalf("stored owner after clearing: %v", got)
	}
}

// Another tenant's service account is not there, for its admins and for an
// operator narrowed to it: by id (a same-named account of their own is
// theirs, by name), on every account and key route, and nothing changes.
func TestServiceAccountsForeignTenant(t *testing.T) {
	f := peopleFixture(t)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO users (id, email, name) VALUES ('usr_t2admin', 'tara@example.com', 'Tara')`)
	execSQL(t, f.s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t2', 'usr_t2admin', 'admin')`)
	t2key := ids.Secret("lux_u")
	execSQL(t, f.s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id) VALUES ('k_t2admin', 't2', 't2admin', $1, '{}', 'usr_t2admin')`, ids.Hash(t2key))
	execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role) VALUES ('sa_bot2', 't2', 'sa-bot', 'member')`)
	// The t2 admin sees their own sa-bot by name: the fixture is live.
	if code, out := f.do(t, t2key, http.MethodGet, "/v1/service-accounts/sa-bot", nil); code != 200 || out["id"] != "sa_bot2" {
		t.Errorf("t2 admin gets t2's sa-bot: %d %v", code, out)
	}
	before := queryOne[string](t, f.s, `SELECT role || description || coalesce(disabled_at::text, '') || (SELECT count(*) FROM api_keys WHERE service_account_id = 'sa_bot' AND revoked_at IS NULL) FROM service_accounts WHERE id = 'sa_bot'`)
	for who, q := range map[string]struct{ key, query string }{
		"t2 admin":             {t2key, ""},
		"t2-narrowed operator": {f.keys["op"], "?tenant=t2"},
	} {
		for _, rt := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/v1/service-accounts/sa_bot", nil},
			{http.MethodPatch, "/v1/service-accounts/sa_bot", map[string]any{"role": "admin", "disabled": true}},
			{http.MethodGet, "/v1/service-accounts/sa_bot/keys", nil},
			{http.MethodPost, "/v1/service-accounts/sa_bot/keys", map[string]any{"name": "stolen"}},
			{http.MethodDelete, "/v1/service-accounts/sa_bot/keys/k_sa", nil},
		} {
			if code, out := f.do(t, q.key, rt.method, rt.path+q.query, rt.body); code != http.StatusNotFound {
				t.Errorf("%s: %s %s: %d %v, want 404", who, rt.method, rt.path, code, out)
			}
		}
	}
	if after := queryOne[string](t, f.s, `SELECT role || description || coalesce(disabled_at::text, '') || (SELECT count(*) FROM api_keys WHERE service_account_id = 'sa_bot' AND revoked_at IS NULL) FROM service_accounts WHERE id = 'sa_bot'`); after != before {
		t.Errorf("t1's account changed: %q, was %q", after, before)
	}
	var who Whoami
	if code := getJSON(t, f.s, f.keys["sa"], "/v1/whoami", &who); code != 200 {
		t.Errorf("t1's account key after: %d", code)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
