package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The console reads these shapes as they are (console/src/api/types.ts and
// console/mockPeopleApi.ts copy them); a change here is a change there.

// DELETE /v1/members/{user}/keys/{key}: admins and owners revoke a
// member's key; an owner's keys only owners and operators; a key not that
// member's in this tenant is 404; key.revoked names the actor.
func TestRevokeMemberKey(t *testing.T) {
	f := peopleFixture(t)
	ctx := context.Background()
	del := func(as, user, key string) int {
		code, _ := f.do(t, f.keys[as], http.MethodDelete, "/v1/members/"+user+"/keys/"+key, nil)
		return code
	}
	// Mia's second key, and one of hers in t2 (where she is no member here).
	execSQL(t, f.s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id, prefix) VALUES ('k_mia2', 't1', 'laptop', 'h2', '{}', 'usr_member', 'lux_u_Mia2')`)
	execSQL(t, f.s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, user_id) VALUES ('k_mia_t2', 't2', 'other', 'h3', '{}', 'usr_member')`)
	for _, as := range []string{"member", "member2", "viewer", "sa"} {
		if code := del(as, "usr_member", "k_mia2"); code != http.StatusForbidden {
			t.Errorf("%s revokes a member's key: %d", as, code)
		}
	}
	for _, c := range []struct{ user, key string }{
		{"usr_member", "k_member2"}, // Max's key, under Mia
		{"usr_member", "k_mia_t2"},  // hers, another tenant
		{"usr_member", "k_sa"},      // a service account's
		{"usr_member", "nope"},
		{"usr_nobody", "k_mia2"},
	} {
		if code := del("admin", c.user, c.key); code != http.StatusNotFound {
			t.Errorf("admin revokes %s under %s: %d", c.key, c.user, code)
		}
	}
	if code := del("admin", "mia@example.com", "lux_u_Mia2"); code != http.StatusNoContent {
		t.Fatalf("admin revokes by email and prefix: %d", code)
	}
	if code := del("admin", "usr_member", "k_mia2"); code != http.StatusNotFound {
		t.Fatalf("revoked twice: %d", code)
	}
	var who Whoami
	if code := getJSON(t, f.s, f.keys["member"], "/v1/whoami", &who); code != 200 {
		t.Fatalf("her other key still works: %d", code)
	}
	if code := del("admin", "usr_owner", "k_owner"); code != http.StatusForbidden {
		t.Fatalf("admin revokes an owner's key: %d", code)
	}
	if code := del("owner", "usr_admin", "k_admin"); code != http.StatusNoContent {
		t.Fatalf("owner revokes an admin's key: %d", code)
	}
	if code := getJSON(t, f.s, f.keys["admin"], "/v1/whoami", &who); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", code)
	}
	if code, _ := f.do(t, f.keys["op"], http.MethodDelete, "/v1/members/usr_owner/keys/k_owner?tenant=t1", nil); code != http.StatusNoContent {
		t.Fatalf("operator revokes an owner's key: %d", code)
	}
	got := queryAll(t, f.s, `SELECT data->>'key' || ' ' || (data->>'user') || ' ' || (data->>'by') FROM run_events WHERE tenant_id = 't1' AND type = 'key.revoked' ORDER BY id`)
	want := []string{"k_mia2 usr_member adam@example.com", "k_admin usr_admin olive@example.com", "k_owner usr_owner k_op"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("key.revoked events: %v, want %v", got, want)
	}
}

// GET /v1/costs?by=member|owner: totals rows keyed user:<id> | sa:<id> |
// (none), with owner (absent on (none)) and group {member|owner: key}.
// runs is filled only with runs=true (or top): the console asks for it.
// The console's test (MemberCost.test.tsx) replays the body logged here.
func TestCostByMemberContract(t *testing.T) {
	s, keys := costLabelFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO users(id,email,name) VALUES('usr_cost','cost@example.com','Cost Member')`)
	execSQL(t, s, ctx, `INSERT INTO memberships(tenant_id,user_id,role) VALUES('t1','usr_cost','member')`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_user_id='usr_cost' WHERE id='r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_sa_id=(SELECT service_account_id FROM api_keys WHERE id='k1') WHERE id='r3'`)
	sa := queryAll(t, s, `SELECT service_account_id || ' ' || (SELECT name FROM service_accounts WHERE id = service_account_id) FROM api_keys WHERE id='k1'`)[0]
	saID, saName, _ := strings.Cut(sa, " ")
	raw := func(q string) []map[string]any {
		code, out := doJSON(t, s, keys["t1"], http.MethodGet, costPath(q), nil)
		if code != 200 {
			t.Fatalf("%s: %d %v", q, code, out)
		}
		b, _ := json.Marshal(out["totals"])
		t.Logf("%s totals: %s", q, b)
		var rows []map[string]any
		for _, r := range out["totals"].([]any) {
			rows = append(rows, r.(map[string]any))
		}
		return rows
	}
	for _, r := range raw("&by=member&family=compute") {
		if _, ok := r["runs"]; ok {
			t.Fatalf("runs without runs=true: %v", r)
		}
	}
	rows := raw("&by=member&family=compute&runs=true")
	byKey := map[string]map[string]any{}
	for _, r := range rows {
		byKey[r["key"].(string)] = r
		if r["runs"] == nil || r["group"].(map[string]any)["member"] != r["key"] {
			t.Errorf("row: %v", r)
		}
	}
	want := map[string]map[string]any{
		"user:usr_cost": {"kind": "user", "id": "usr_cost", "name": "Cost Member", "email": "cost@example.com"},
		"sa:" + saID:    {"kind": "serviceAccount", "id": saID, "name": saName},
	}
	for k, o := range want {
		if !reflect.DeepEqual(byKey[k]["owner"], o) {
			t.Errorf("%s owner: %v, want %v", k, byKey[k]["owner"], o)
		}
		if byKey[k]["runs"] != float64(1) {
			t.Errorf("%s runs: %v", k, byKey[k]["runs"])
		}
	}
	if none := byKey["(none)"]; none == nil || none["owner"] != nil || none["amount"] != "100" || none["currency"] != "USD" {
		t.Errorf("(none): %v", none)
	}
	if len(byKey) != 3 {
		t.Errorf("rows: %v", byKey)
	}
}

// An operator who is also a member of a tenant keeps full operator
// capability with ?tenant= for that tenant (docs/security.md): no role
// from the membership narrows them, and whoami says operator, no role.
func TestOperatorMemberKeepsOperator(t *testing.T) {
	f, access := accessFixture(t)
	s := f.s
	execSQL(t, s, context.Background(), `INSERT INTO memberships (tenant_id, user_id, role) SELECT 't2', id, 'viewer' FROM users WHERE email = 'op@example.com'`)
	code, who := accessDo(t, s, access.sign("op@example.com"), http.MethodGet, "/v1/whoami?tenant=t2", nil)
	if code != 200 || who["operator"] != true || who["role"] != nil {
		t.Fatalf("whoami: %d %v", code, who)
	}
	// A viewer could not do these; the operator does.
	if code, out := accessDo(t, s, access.sign("op@example.com"), http.MethodPost, "/v1/runs?tenant=t2", rawJSON(minimalRun)); code != http.StatusCreated {
		t.Fatalf("operator (a viewer of t2) submits: %d %v", code, out)
	}
	if code, out := accessDo(t, s, access.sign("op@example.com"), http.MethodPatch, "/v1/members/usr_member?tenant=t1", map[string]any{"role": "admin"}); code != 200 {
		t.Fatalf("operator changes a role: %d %v", code, out)
	}
}

// GET /v1/tenants: owners are the owners as {id, name, email} (name empty
// when the person has none) by email, with ownersCount and membersCount; POST /v1/tenants
// answers {id, tenantId, name, invitation?}.
func TestTenantPeopleContract(t *testing.T) {
	f, _ := accessFixture(t)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO users (id, email, name) VALUES ('usr_noname', 'zed@example.com', '')`)
	execSQL(t, f.s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', 'usr_noname', 'owner')`)
	code, out := f.do(t, f.keys["op"], http.MethodGet, "/v1/tenants", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, out)
	}
	byName := map[string]map[string]any{}
	for _, x := range out["tenants"].([]any) {
		m := x.(map[string]any)
		byName[m["name"].(string)] = m
	}
	t1, t2 := byName["t1"], byName["t2"]
	// The fixture's five people, zed, and op@example.com (an operator, admin of the default tenant t1).
	if t1["ownersCount"] != float64(2) || t1["membersCount"] != float64(7) {
		t.Fatalf("t1 counts: %v", t1)
	}
	want := []any{
		map[string]any{"id": "usr_owner", "name": "owner person", "email": "olive@example.com"},
		map[string]any{"id": "usr_noname", "name": "", "email": "zed@example.com"},
	}
	if got := t1["owners"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("t1 owners ({id, name, email}, by email): %#v", got)
	}
	if t2["ownersCount"] != float64(0) || !reflect.DeepEqual(t2["owners"], []any{}) {
		t.Fatalf("t2 has no owner: %v", t2)
	}
	if _, ok := t1["serviceAccounts"]; ok {
		t.Fatalf("serviceAccounts is not sent: %v", t1)
	}
	code, out = f.do(t, f.keys["op"], http.MethodPost, "/v1/tenants", map[string]any{"name": "t3", "ownerEmail": "first@example.com"})
	if code >= 300 || out["id"] == nil || out["id"] != out["tenantId"] || out["name"] != "t3" {
		t.Fatalf("create: %d %v", code, out)
	}
	if _, ok := out["tenant"]; ok {
		t.Fatalf("create answers no tenant object: %v", out)
	}
	inv := out["invitation"].(map[string]any)
	if inv["email"] != "first@example.com" || inv["role"] != "owner" || inv["url"] == nil {
		t.Fatalf("first owner's invitation: %v", inv)
	}
	code, out = f.do(t, f.keys["op"], http.MethodPost, "/v1/tenants", map[string]any{"name": "t4"})
	if _, ok := out["invitation"]; code >= 300 || ok {
		t.Fatalf("create without owner: %d %v", code, out)
	}
}
