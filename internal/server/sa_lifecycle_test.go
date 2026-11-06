package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"

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
	expiryBefore := queryAll(t, f.s, `SELECT id || ':' || coalesce(expires_at::text, 'NULL') FROM api_keys WHERE id LIKE 'k_ext%' ORDER BY id`)
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
	if after := queryAll(t, f.s, `SELECT id || ':' || coalesce(expires_at::text, 'NULL') FROM api_keys WHERE id LIKE 'k_ext%' ORDER BY id`); stringify(after) != stringify(expiryBefore) {
		t.Fatalf("expiry changed: before %v after %v", expiryBefore, after)
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
	holder := lifecycleHolder(t, f, "lux_members:t1", false)
	codes := make(chan int, 2)
	for _, email := range []string{"mia@example.com", "max@example.com"} {
		go func(email string) {
			code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/external-app/convert", map[string]any{"email": email})
			codes <- code
		}(email)
	}
	waitLifecycleLock(t, f, "lux_members:t1", 2)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
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

// lifecycleHolder and waitLifecycleLock establish exact advisory-lock barriers,
// not merely overlapping goroutines. Observation is bounded and tenant/db scoped.
func lifecycleHolder(t *testing.T, f *peopleFix, key string, shared bool) pgx.Tx {
	t.Helper()
	tx, err := f.s.db.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	fn := "pg_advisory_xact_lock"
	if shared {
		fn += "_shared"
	}
	if _, err := tx.Exec(context.Background(), `SELECT `+fn+`(hashtext($1))`, key); err != nil {
		t.Fatal(err)
	}
	return tx
}

func waitLifecycleLock(t *testing.T, f *peopleFix, key string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var count int
		err := f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
				AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND classid = CASE WHEN hashtext($1) < 0 THEN 4294967295 ELSE 0 END::oid
				AND objid = (hashtext($1)::bigint & 4294967295)::oid`, key).Scan(&count)
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("expected %d waiters on %s, got %d", n, key, count)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type lifecycleBody struct {
	io.Reader
	entered chan struct{}
	resume  chan struct{}
}

func (b *lifecycleBody) Read(p []byte) (int, error) {
	if b.entered != nil {
		close(b.entered)
		b.entered = nil
		<-b.resume
	}
	return b.Reader.Read(p)
}
func (b *lifecycleBody) Close() error { return nil }

func TestServiceAccountKeyCallerExpiresWhileQueued(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			f := peopleFixture(t)
			legacyAccount(t, f)
			execSQL(t, f.s, context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE id = 'k_ext2'`)
			execSQL(t, f.s, context.Background(), `UPDATE service_accounts SET role = 'admin' WHERE id = 'sa_bot'`)
			execSQL(t, f.s, context.Background(), `UPDATE api_keys SET expires_at = clock_timestamp() + interval '3 seconds' WHERE id = 'k_sa'`)
			holder := lifecycleHolder(t, f, "lux_keys:t1:sa_bot", false)
			path := "/v1/service-accounts/sa_ext/keys"
			var payload any = map[string]any{"name": "expired-caller-replacement"}
			if method == http.MethodDelete {
				path += "/k_ext"
				payload = nil
			}
			done := make(chan int, 1)
			go func() { code, _ := f.do(t, f.keys["sa"], method, path, payload); done <- code }()
			waitLifecycleLock(t, f, "lux_keys:t1:sa_bot", 1)
			if _, err := holder.Exec(context.Background(), `SELECT pg_sleep(3.1)`); err != nil {
				t.Fatal(err)
			}
			if !queryOne[bool](t, f.s, `SELECT expires_at < clock_timestamp() FROM api_keys WHERE id = 'k_sa'`) {
				t.Fatal("caller has not expired")
			}
			if err := holder.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				t.Logf("expired caller %s returned %d; replacements=%d; revoked=%v", method, code,
					queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE name = 'expired-caller-replacement'`),
					queryOne[bool](t, f.s, `SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = 'k_ext'`))
				if code != http.StatusUnauthorized {
					t.Errorf("expired caller authorized: %d, want 401", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request blocked")
			}
		})
	}
}

func TestServiceAccountKeyCallerPausedBody(t *testing.T) {
	for _, action := range []string{"revoke", "convert", "delete", "disable", "demote", "readonly"} {
		t.Run(action, func(t *testing.T) {
			f := peopleFixture(t)
			legacyAccount(t, f)
			execSQL(t, f.s, context.Background(), `UPDATE service_accounts SET role = 'admin' WHERE id = 'sa_bot'`)
			body := &lifecycleBody{Reader: strings.NewReader(`{"name":"replacement"}`), entered: make(chan struct{}), resume: make(chan struct{})}
			entered := body.entered
			req := httptest.NewRequest(http.MethodPost, "/v1/service-accounts/sa_ext/keys", nil)
			req.Body = body
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+f.keys["sa"])
			done := make(chan int, 1)
			go func() { w := httptest.NewRecorder(); f.s.Handler().ServeHTTP(w, req); done <- w.Code }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("body not reached")
			}
			method, path, want := http.MethodPatch, "/v1/service-accounts/sa_bot", 200
			var payload any
			switch action {
			case "revoke":
				method, path, want = http.MethodDelete, path+"/keys/k_sa", 204
			case "convert":
				method, path, payload = http.MethodPost, path+"/convert", map[string]any{"email": "mia@example.com"}
			case "delete":
				method, want = http.MethodDelete, 204
			case "disable":
				payload = map[string]any{"disabled": true}
			case "demote":
				payload = map[string]any{"role": "viewer"}
			case "readonly":
				execSQL(t, f.s, context.Background(), `UPDATE api_keys SET read_only = true WHERE id = 'k_sa'`)
			}
			if action != "readonly" {
				if code, out := f.do(t, f.keys["owner"], method, path, payload); code != want {
					close(body.resume)
					t.Fatalf("%s: %d %v", action, code, out)
				}
			}
			close(body.resume)
			want = 401
			if action == "demote" || action == "readonly" {
				want = 404
			}
			select {
			case code := <-done:
				if code != want {
					t.Fatalf("stale caller: %d, want %d", code, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("key create blocked")
			}
			if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE name = 'replacement' AND revoked_at IS NULL`); n != 0 {
				t.Fatalf("minted %d replacement keys", n)
			}
		})
	}
}

func TestServiceAccountKeyCallerQueued(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			f := peopleFixture(t)
			legacyAccount(t, f)
			execSQL(t, f.s, context.Background(), `UPDATE service_accounts SET role = 'admin' WHERE id = 'sa_bot'`)
			holder := lifecycleHolder(t, f, "lux_keys:t1:sa_bot", false)
			path := "/v1/service-accounts/sa_ext/keys"
			var payload any = map[string]any{"name": "queued"}
			if method == http.MethodDelete {
				path += "/k_ext"
				payload = nil
			}
			done := make(chan int, 1)
			go func() { code, _ := f.do(t, f.keys["sa"], method, path, payload); done <- code }()
			waitLifecycleLock(t, f, "lux_keys:t1:sa_bot", 1)
			if _, err := holder.Exec(context.Background(), `SELECT set_config('lux.system', 'on', true)`); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(context.Background(), `UPDATE service_accounts SET role = 'viewer' WHERE id = 'sa_bot'`); err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != 404 {
					t.Fatalf("demoted caller: %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued operation blocked")
			}
			if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE name = 'queued'`); n != 0 {
				t.Fatalf("created %d keys", n)
			}
			if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys WHERE id = 'k_ext' AND revoked_at IS NULL`); n != 1 {
				t.Fatal("revoked despite demotion")
			}
		})
	}
}

func TestServiceAccountCrossKeyLockOrder(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	execSQL(t, f.s, context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE id = 'k_ext2'`)
	execSQL(t, f.s, context.Background(), `UPDATE service_accounts SET role = 'admin' WHERE id IN ('sa_bot', 'sa_ext')`)
	holder := lifecycleHolder(t, f, "lux_keys:t1:sa_bot", false)
	done := make(chan int, 2)
	go func() {
		code, _ := f.do(t, f.keys["sa"], http.MethodPost, "/v1/service-accounts/sa_ext/keys", map[string]any{"name": "cross-a"})
		done <- code
	}()
	go func() {
		code, _ := f.do(t, f.keys["ext"], http.MethodDelete, "/v1/service-accounts/sa_bot/keys/k_sa", nil)
		done <- code
	}()
	// Both must wait on the SAME first sorted ID, rather than holding their
	// respective targets and deadlocking while waiting for the other caller.
	waitLifecycleLock(t, f, "lux_keys:t1:sa_bot", 2)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	codes := []int{}
	for range 2 {
		select {
		case code := <-done:
			codes = append(codes, code)
		case <-time.After(5 * time.Second):
			t.Fatal("cross operations deadlocked")
		}
	}
	// If revocation wins the create must reauthenticate; otherwise it commits
	// before revocation. Either serial order preserves current permissions.
	if !((codes[0] == 204 && (codes[1] == 201 || codes[1] == 401)) || (codes[1] == 204 && (codes[0] == 201 || codes[0] == 401))) {
		t.Fatalf("cross results %v", codes)
	}
}

func TestServiceAccountPausedSubmit(t *testing.T) {
	for _, action := range []string{"delete", "convert"} {
		t.Run(action, func(t *testing.T) {
			f := peopleFixture(t)
			body := &lifecycleBody{Reader: strings.NewReader(`{"image":{"ref":"alpine"},"workload":{"command":["true"]}}`), entered: make(chan struct{}), resume: make(chan struct{})}
			entered := body.entered
			req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
			req.Body = body
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+f.keys["sa"])
			done := make(chan int, 1)
			go func() { w := httptest.NewRecorder(); f.s.Handler().ServeHTTP(w, req); done <- w.Code }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("body not reached")
			}
			method, path := http.MethodDelete, "/v1/service-accounts/sa_bot"
			var payload any
			want := 204
			if action == "convert" {
				method, path, payload, want = http.MethodPost, path+"/convert", map[string]any{"email": "mia@example.com"}, 200
			}
			code, out := f.do(t, f.keys["admin"], method, path, payload)
			close(body.resume)
			if code != want {
				t.Fatalf("lifecycle %d %v", code, out)
			}
			select {
			case code = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("submit blocked")
			}
			if code != 401 {
				t.Fatalf("stale submit %d, want 401", code)
			}
			if n := queryOne[int64](t, f.s, `SELECT count(*) FROM runs`); n != 0 {
				t.Fatalf("inserted %d Runs", n)
			}
			code, _ = f.do(t, f.keys["sa"], http.MethodGet, "/v1/whoami", nil)
			if action == "delete" && code != 401 {
				t.Fatalf("deleted key %d", code)
			}
			if action == "convert" && code != 200 {
				t.Fatalf("converted retry %d", code)
			}
		})
	}
}

func TestServiceAccountCapSerialization(t *testing.T) {
	f := peopleFixture(t)
	for i := 0; i < 3; i++ {
		f.addKey(t, "extra"+string(rune('a'+i)), "user_id", "usr_member")
	}
	holder := lifecycleHolder(t, f, "lux_keys:t1:usr_member", false)
	converted := make(chan int, 1)
	go func() {
		code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/sa_bot/convert", map[string]any{"email": "mia@example.com"})
		converted <- code
	}()
	waitLifecycleLock(t, f, "lux_keys:t1:usr_member", 1)
	created := make(chan int, 1)
	go func() {
		code, _ := f.do(t, f.keys["admin"], http.MethodPost, "/v1/service-accounts/sa_bot/keys", map[string]any{"name": "queued"})
		created <- code
	}()
	waitLifecycleLock(t, f, "lux_keys:t1:sa_bot", 1)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := <-converted; code != 200 {
		t.Fatalf("convert %d", code)
	}
	if code := <-created; code != 404 {
		t.Fatalf("queued creator %d", code)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM api_keys k WHERE user_id = 'usr_member' AND `+activeKey); n != 5 {
		t.Fatalf("cap %d", n)
	}
}

func TestServiceAccountInsertWinsLifecycle(t *testing.T) {
	for _, action := range []string{"delete", "convert"} {
		t.Run(action, func(t *testing.T) {
			f := peopleFixture(t)
			// Hold the Run table: submit acquires its shared SA lock, then
			// blocks at INSERT. Observe both locks before releasing it.
			holder, err := f.s.db.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(context.Background(), `LOCK TABLE runs IN SHARE MODE`); err != nil {
				t.Fatal(err)
			}
			submitted := make(chan int, 1)
			go func() {
				code, _ := f.do(t, f.keys["sa"], http.MethodPost, "/v1/runs", map[string]any{"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"true"}}})
				submitted <- code
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				var blocked bool
				err := f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation = 'runs'::regclass AND mode = 'RowExclusiveLock' AND NOT granted)`).Scan(&blocked)
				})
				if err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("submit did not reach insert")
				case <-time.After(5 * time.Millisecond):
				}
			}
			lifecycle := make(chan int, 1)
			go func() {
				method, path := http.MethodDelete, "/v1/service-accounts/sa_bot"
				var body any
				if action == "convert" {
					method, path, body = http.MethodPost, path+"/convert", map[string]any{"email": "mia@example.com"}
				}
				code, _ := f.do(t, f.keys["admin"], method, path, body)
				lifecycle <- code
			}()
			waitLifecycleLock(t, f, "lux_keys:t1:sa_bot", 1)
			if err := holder.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			if code := <-submitted; code != 201 {
				t.Fatalf("submit %d", code)
			}
			want := 409
			if action == "convert" {
				want = 200
			}
			if code := <-lifecycle; code != want {
				t.Fatalf("%s %d", action, code)
			}
			if action == "convert" {
				if n := queryOne[int64](t, f.s, `SELECT count(*) FROM runs WHERE owner_user_id = 'usr_member' AND owner_sa_id IS NULL`); n != 1 {
					t.Fatal("new Run did not move")
				}
			}
		})
	}
}

func TestServiceAccountRenameConcurrent(t *testing.T) {
	f := peopleFixture(t)
	legacyAccount(t, f)
	holder, err := f.s.db.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(context.Background(), `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(context.Background(), `SELECT id FROM service_accounts WHERE id = 'sa_ext' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 2)
	go func() {
		code, _ := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/sa_ext", map[string]any{"name": "first"})
		codes <- code
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var n int
		err := f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("rename did not reach row lock")
		case <-time.After(5 * time.Millisecond):
		}
	}
	go func() {
		code, _ := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/service-accounts/sa_ext", map[string]any{"name": "second"})
		codes <- code
	}()
	waitLifecycleLock(t, f, "lux_keys:t1:sa_ext", 1)
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := <-codes; code != 200 {
			t.Fatalf("rename %d", code)
		}
	}
	if name := queryOne[string](t, f.s, `SELECT name FROM api_keys WHERE id = 'k_ext'`); name != "second" {
		t.Fatalf("key name %s", name)
	}
	if ev := queryAll(t, f.s, `SELECT data->>'from' || '>' || (data->>'to') FROM run_events WHERE type = 'service_account.updated' ORDER BY id`); strings.Join(ev, ",") != "external-app>first,first>second" {
		t.Fatalf("events %v", ev)
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
