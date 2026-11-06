package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

func TestMembersOwnerRules(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	for _, key := range []string{"viewer", "member", "sa"} {
		if code, _ := f.do(t, f.keys[key], http.MethodPatch, "/v1/members/usr_member2", map[string]any{"role": "admin"}); code != 403 {
			t.Errorf("%s changes role: %d", key, code)
		}
	}
	if code, _ := f.do(t, f.keys["admin"], http.MethodPatch, "/v1/members/usr_member2", map[string]any{"role": "admin"}); code != 200 {
		t.Fatal(code)
	}
	for _, action := range []struct {
		method, path string
		body         any
	}{{"PATCH", "/v1/members/usr_owner", map[string]any{"role": "admin"}}, {"DELETE", "/v1/members/usr_owner", nil}, {"PATCH", "/v1/members/usr_member2", map[string]any{"role": "owner"}}} {
		if code, _ := f.do(t, f.keys["admin"], action.method, action.path, action.body); code != 403 {
			t.Errorf("admin owner action: %d", code)
		}
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		code, out := f.do(t, f.keys["owner"], method, "/v1/members/usr_owner", map[string]any{"role": "admin"})
		if c, _ := errCode(out); code != 409 || c != "last_owner" {
			t.Errorf("last owner %s: %d %v", method, code, out)
		}
	}
	if code, _ := f.do(t, f.keys["owner"], "PATCH", "/v1/members/usr_admin", map[string]any{"role": "owner"}); code != 200 {
		t.Fatal(code)
	}
	if code, _ := f.do(t, f.keys["owner"], "PATCH", "/v1/members/usr_owner", map[string]any{"role": "admin"}); code != 200 {
		t.Fatal(code)
	}
	// Removing someone revokes their keys and leaves their Run attributed.
	// A second key of theirs in t1, and one in t2 (another tenant's: kept).
	execSQL(t, f.s, context.Background(), `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t2', 'usr_member', 'member')`)
	second := ids.Secret("lux_u")
	execSQL(t, f.s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id, read_only) VALUES
		('k_member_b', 't1', 'b', $1, '{}', 'usr_member', true), ('k_member_t2', 't2', 't2', $2, '{}', 'usr_member', false)`, ids.Hash(second), ids.Hash(ids.Secret("lux_u")))
	code, out := f.do(t, f.keys["admin"], "DELETE", "/v1/members/usr_member", nil)
	if code != 204 {
		t.Fatalf("remove: %d %v", code, out)
	}
	if code, _ := f.do(t, f.keys["member"], "GET", "/v1/runs", nil); code != 401 {
		t.Errorf("removed member's key: %d", code)
	}
	if got := queryAll(t, f.s, `SELECT id || ':' || (revoked_at IS NOT NULL) FROM api_keys WHERE user_id = 'usr_member' ORDER BY id`); stringify(got) != `["k_member:true","k_member_b:true","k_member_t2:false"]` {
		t.Errorf("keys after removal (id:revoked): %v", got)
	}
	// Back in the tenant, the old secrets stay dead.
	if code, out := f.do(t, f.keys["op"], "POST", "/v1/members?tenant=t1", map[string]any{"email": "mia@example.com", "role": "member"}); code >= 300 {
		t.Fatalf("add back: %d %v", code, out)
	}
	for name, secret := range map[string]string{"first": f.keys["member"], "second": second} {
		if code, _ := f.do(t, secret, "GET", "/v1/runs", nil); code != 401 {
			t.Errorf("re-added member's old %s key: %d, want 401", name, code)
		}
	}
	if owner := queryOne[*string](t, f.s, `SELECT owner_user_id FROM runs WHERE id='r_mia'`); owner == nil || *owner != "usr_member" {
		t.Error("Run lost its owner")
	}
	if owner := queryOne[*string](t, f.s, `SELECT owner_user_id FROM service_accounts WHERE id='sa_bot'`); owner != nil {
		t.Error("SA kept removed owner")
	}
	// Key counts and key routes only self/admins.
	code, out = f.do(t, f.keys["viewer"], "GET", "/v1/members/usr_admin", nil)
	if code != 200 || out["keyCount"] != nil {
		t.Errorf("key count leak: %d %v", code, out)
	}
	if code, _ := f.do(t, f.keys["viewer"], "GET", "/v1/members/usr_admin/keys", nil); code != 404 {
		t.Error(code)
	}
	if code, out := f.do(t, f.keys["viewer"], "GET", "/v1/members/usr_viewer", nil); code != 200 || out["keyCount"] == nil {
		t.Errorf("self: %d %v", code, out)
	}
	// Operators can remove the last owner.
	if code, out := f.do(t, f.keys["op"], "DELETE", "/v1/members/usr_admin?tenant=t1", nil); code != 204 {
		t.Errorf("operator last owner: %d %v", code, out)
	}
	_ = context.Background
}

// POST /v1/members {upsert:true}: operators only; a non-member is added with
// the role, a member gets it (member.role_changed); without upsert, 409.
func TestAddMemberUpsert(t *testing.T) {
	f := peopleFixture(t)
	for _, key := range []string{"owner", "admin"} {
		if code, out := f.do(t, f.keys[key], "POST", "/v1/members", map[string]any{"email": "new@example.com", "role": "owner", "upsert": true}); code != 403 {
			t.Errorf("%s upserts: %d %v", key, code, out)
		}
	}
	code, out := f.do(t, f.keys["op"], "POST", "/v1/members?tenant=t2", map[string]any{"email": "New@Example.com", "role": "owner", "upsert": true})
	if code != 200 || out["role"] != "owner" || out["email"] != "New@Example.com" {
		t.Fatalf("add as owner: %d %v", code, out)
	}
	if code, out := f.do(t, f.keys["op"], "POST", "/v1/members?tenant=t1", map[string]any{"email": "mia@example.com", "role": "owner"}); code != 409 {
		t.Errorf("no upsert, a member: %d %v", code, out)
	}
	code, out = f.do(t, f.keys["op"], "POST", "/v1/members?tenant=t1", map[string]any{"email": "MIA@example.com", "role": "owner", "upsert": true})
	if code != 200 || out["role"] != "owner" || out["id"] != "usr_member" {
		t.Fatalf("promote: %d %v", code, out)
	}
	if got := queryAll(t, f.s, `SELECT type || ':' || (data->>'user') || ':' || coalesce(data->>'from', '') || ':' || coalesce(data->>'to', data->>'role') || ':' || (data->>'by') FROM run_events WHERE type LIKE 'member.%' ORDER BY id`); stringify(got) != `["member.joined:`+queryOne[string](t, f.s, `SELECT id FROM users WHERE email='New@Example.com'`)+`::owner:k_op","member.role_changed:usr_member:member:owner:k_op"]` {
		t.Errorf("events: %v", got)
	}
	// Upsert to the same role: no change, no event.
	if code, _ := f.do(t, f.keys["op"], "POST", "/v1/members?tenant=t1", map[string]any{"email": "mia@example.com", "role": "owner", "upsert": true}); code != 200 {
		t.Error(code)
	}
	if n := queryOne[int64](t, f.s, `SELECT count(*) FROM run_events WHERE type LIKE 'member.%'`); n != 2 {
		t.Errorf("events after a no-op upsert: %d", n)
	}
}

// An owner changes their own role while another owner exists; the last owner
// who is not an operator cannot (409 last_owner); an operator always can.
func TestOwnerChangesOwnRole(t *testing.T) {
	f := peopleFixture(t)
	code, out := f.do(t, f.keys["owner"], "PATCH", "/v1/members/olive@example.com", map[string]any{"role": "admin"})
	if c, _ := errCode(out); code != 409 || c != "last_owner" {
		t.Fatalf("last owner demotes self: %d %v", code, out)
	}
	if code, out := f.do(t, f.keys["owner"], "PATCH", "/v1/members/usr_admin", map[string]any{"role": "owner"}); code != 200 {
		t.Fatalf("second owner: %d %v", code, out)
	}
	if code, out := f.do(t, f.keys["owner"], "PATCH", "/v1/members/olive@example.com", map[string]any{"role": "admin"}); code != 200 || out["role"] != "admin" {
		t.Fatalf("owner demotes self with another owner: %d %v", code, out)
	}
	// Now the only owner, adam, as an operator would be allowed: the operator demotes them.
	if code, out := f.do(t, f.keys["op"], "PATCH", "/v1/members/usr_admin?tenant=t1", map[string]any{"role": "member"}); code != 200 {
		t.Fatalf("operator demotes the last owner: %d %v", code, out)
	}
}
