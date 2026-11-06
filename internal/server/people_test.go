package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

// peopleFixture: tenants t1 and t2; in t1 an owner, an admin, a member and
// a viewer (each with a personal key), a service account sa-bot (member,
// owned by the member) with a key, and an operator key. Keys by name.
type peopleFix struct {
	s    *Server
	keys map[string]string
}

var fixtureUsers = []struct{ id, email, role string }{
	{"usr_owner", "olive@example.com", "owner"},
	{"usr_admin", "adam@example.com", "admin"},
	{"usr_member", "mia@example.com", "member"},
	{"usr_member2", "max@example.com", "member"},
	{"usr_viewer", "vic@example.com", "viewer"},
}

func peopleFixture(t *testing.T) *peopleFix {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	f := &peopleFix{s: s, keys: map[string]string{}}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	for _, u := range fixtureUsers {
		execSQL(t, s, ctx, `INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`, u.id, u.email, u.role+" person")
		execSQL(t, s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', $1, $2)`, u.id, u.role)
		f.addKey(t, strings.TrimPrefix(u.id, "usr_"), `user_id`, u.id)
	}
	execSQL(t, s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role, owner_user_id) VALUES ('sa_bot', 't1', 'sa-bot', 'member', 'usr_member')`)
	f.addKey(t, "sa", `service_account_id`, "sa_bot")
	f.keys["op"] = ids.Secret("lux")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k_op', NULL, 'ops', $1, ARRAY['operator'])`, ids.Hash(f.keys["op"]))
	return f
}

// addKey gives the person or service account a key named name.
func (f *peopleFix) addKey(t *testing.T, name, col, id string) {
	t.Helper()
	key := ids.Secret("lux_u")
	f.keys[name] = key
	execSQL(t, f.s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, `+col+`) VALUES ($1, 't1', $2, $3, '{}', $4)`,
		"k_"+name, name, ids.Hash(key), id)
}

// do sends a JSON request with a key and decodes the answer.
func (f *peopleFix) do(t *testing.T, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, f.s, key, method, path, body)
}

func doJSON(t *testing.T, s *Server, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, jsonBody(t, body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, decodeMap(w.Body.Bytes())
}

type rawJSON = json.RawMessage

func jsonBody(t *testing.T, body any) *bytes.Reader {
	t.Helper()
	if body == nil {
		return bytes.NewReader(nil)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func decodeMap(b []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// A person's key acts with their role in the key's tenant; it stops working
// when they are removed, disabled, or the key expires; a read-only key
// reads only, whatever the role.
func TestPersonalKeyAuth(t *testing.T) {
	f := peopleFixture(t)
	s, ctx := f.s, context.Background()
	for _, tc := range []struct {
		key   string
		scope []string
		role  string
	}{
		{"owner", []string{"admin"}, "owner"}, {"admin", []string{"admin"}, "admin"},
		{"member", []string{"run"}, "member"}, {"viewer", []string{"read"}, "viewer"}, {"sa", []string{"run"}, "member"},
	} {
		var who Whoami
		if code := getJSON(t, s, f.keys[tc.key], "/v1/whoami", &who); code != 200 || len(who.Scopes) != 1 || who.Scopes[0] != tc.scope[0] || who.Role != tc.role {
			t.Errorf("%s: %d %+v", tc.key, code, who)
		}
	}
	// A role change counts at once.
	execSQL(t, s, ctx, `UPDATE memberships SET role = 'viewer' WHERE user_id = 'usr_member'`)
	if code, _ := postJSON(t, s, f.keys["member"], "/v1/runs", minimalRun); code != http.StatusForbidden {
		t.Errorf("demoted member submits: %d", code)
	}
	execSQL(t, s, ctx, `UPDATE memberships SET role = 'member' WHERE user_id = 'usr_member'`)
	if code, out := postJSON(t, s, f.keys["member"], "/v1/runs", minimalRun); code != http.StatusCreated {
		t.Errorf("member submits: %d %v", code, out)
	}
	// Read-only: an admin's key that reads.
	ro := ids.Secret("lux_u")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id, read_only) VALUES ('k_ro', 't1', 'ro', $1, '{}', 'usr_admin', true)`, ids.Hash(ro))
	var who Whoami
	if code := getJSON(t, s, ro, "/v1/whoami", &who); code != 200 {
		t.Errorf("read-only key reads: %d", code)
	}
	if code, out := postJSON(t, s, ro, "/v1/runs", minimalRun); code != http.StatusForbidden {
		t.Errorf("read-only admin key submits: %d %v", code, out)
	}
	if code, _ := doJSON(t, s, ro, http.MethodPost, "/v1/pools", map[string]any{"name": "p", "provider": "static"}); code != http.StatusForbidden {
		t.Errorf("read-only admin key sets a pool: %d", code)
	}
	// Expired.
	execSQL(t, s, ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = 'k_ro'`)
	if code := getJSON(t, s, ro, "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Errorf("expired key: %d", code)
	}
	// A disabled person, a disabled service account.
	execSQL(t, s, ctx, `UPDATE users SET disabled_at = now() WHERE id = 'usr_viewer'`)
	if code := getJSON(t, s, f.keys["viewer"], "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Errorf("disabled person's key: %d", code)
	}
	execSQL(t, s, ctx, `UPDATE service_accounts SET disabled_at = now() WHERE id = 'sa_bot'`)
	if code := getJSON(t, s, f.keys["sa"], "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Errorf("disabled account's key: %d", code)
	}
	// Removed from the tenant (the membership row gone, as an operator might).
	execSQL(t, s, ctx, `DELETE FROM memberships WHERE user_id = 'usr_member2'`)
	if code := getJSON(t, s, f.keys["member2"], "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Errorf("removed member's key: %d", code)
	}
}
