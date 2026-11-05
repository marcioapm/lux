package server

import (
	"context"
	"net/http"
	"testing"
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
	code, out := f.do(t, f.keys["admin"], "DELETE", "/v1/members/usr_member", nil)
	if code != 204 {
		t.Fatalf("remove: %d %v", code, out)
	}
	if code, _ := f.do(t, f.keys["member"], "GET", "/v1/runs", nil); code != 401 {
		t.Errorf("removed member's key: %d", code)
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
