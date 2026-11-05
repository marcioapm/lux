package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// The console reads these shapes as they are (console/src/api/types.ts and
// console/mockPeopleApi.ts copy them); a change here is a change there.

// GET /v1/tenants: owners are the owners' display names (name, else
// email) by email, with ownersCount and membersCount; POST /v1/tenants
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
	if got := t1["owners"]; !reflect.DeepEqual(got, []any{"owner person", "zed@example.com"}) {
		t.Fatalf("t1 owners (names, else email, by email): %#v", got)
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
