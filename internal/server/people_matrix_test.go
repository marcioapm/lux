package server

import (
	"net/http"
	"testing"
)

// Each role against each action class, through the HTTP handlers.
func TestRoleActionMatrix(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	for _, tc := range []struct {
		key                                          string
		read, submit, own, other, members, sa, owner bool
	}{
		{"viewer", true, false, false, false, false, false, false},
		{"member", true, true, true, false, false, false, false},
		{"admin", true, true, true, true, true, true, false},
		{"owner", true, true, true, true, true, true, true},
		{"sa", true, true, true, false, false, false, false},
		{"op", true, true, true, true, true, true, true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			key := f.keys[tc.key]
			suffix := ""
			if tc.key == "op" {
				suffix = "?tenant=t1"
			}
			own := "r_mia"
			if tc.key == "sa" {
				own = "r_sa"
			} else if tc.key == "admin" {
				own = "r_none"
			} else if tc.key == "owner" {
				own = "r_olive"
			}
			for _, action := range []struct {
				name, method, path string
				body               any
				allowed            bool
			}{
				{"read", "GET", "/v1/runs", nil, tc.read},
				{"submit", "POST", "/v1/runs", rawJSON(minimalRun), tc.submit},
				{"own Run", "PATCH", "/v1/runs/" + own, map[string]any{"name": "own"}, tc.own},
				{"another Run", "PATCH", "/v1/runs/r_none", map[string]any{"name": "another"}, tc.other},
				{"manage members", "PATCH", "/v1/members/usr_member2", map[string]any{"role": "member"}, tc.members},
				{"manage service accounts", "POST", "/v1/service-accounts", map[string]any{"name": "matrix-" + tc.key, "role": "member"}, tc.sa},
				{"owner capability", "PATCH", "/v1/members/usr_owner", map[string]any{"role": "owner"}, tc.owner},
			} {
				code, out := f.do(t, key, action.method, action.path+suffix, action.body)
				if action.allowed && code/100 != 2 {
					t.Errorf("%s: %d %v", action.name, code, out)
				}
				if !action.allowed && code != http.StatusForbidden {
					t.Errorf("%s allowed: %d %v", action.name, code, out)
				}
			}
		})
	}
}

// The feed carries all people events in the tenant, with the actor, and
// no Run or server subject. Other tenants never receive them.
func TestPeopleEventsOnTenantFeed(t *testing.T) {
	f := peopleFixture(t)
	if code, out := f.do(t, f.keys["member"], "POST", "/v1/me/keys", map[string]any{"name": "feed"}); code != 201 {
		t.Fatal(code, out)
	}
	if code, out := f.do(t, f.keys["admin"], "PATCH", "/v1/members/usr_member2", map[string]any{"role": "viewer"}); code != 200 {
		t.Fatal(code, out)
	}
	code, out := f.do(t, f.keys["viewer"], "GET", "/v1/members", nil)
	if code != 200 || len(out["members"].([]any)) != 5 {
		t.Fatal(code, out)
	}
	p, err := f.s.authKey(t.Context(), f.keys["viewer"], "read")
	if err != nil {
		t.Fatal(err)
	}
	feed, err := f.s.feedAfter(t.Context(), p, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range feed {
		seen[ev.Type] = true
		if ev.Data["by"] == "" || ev.Subject == "" || ev.RunID != nil {
			t.Errorf("bad event: %+v", ev)
		}
	}
	if !seen["key.created"] || !seen["member.role_changed"] {
		t.Fatal(seen)
	}
	p.TenantID = "t2"
	feed, err = f.s.feedAfter(t.Context(), p, 0)
	if err != nil || len(feed) != 0 {
		t.Fatalf("foreign feed %v %v", feed, err)
	}
}
