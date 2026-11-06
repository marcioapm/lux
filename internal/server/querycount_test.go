package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/store"
)

// tracedRequest serves one request through a pool recording every SELECT
// it runs (as feedRecorder does), and returns the status and statements;
// s.db is restored before it returns.
func tracedRequest(t *testing.T, s *Server, key, method, path string) (int, []feedStatement) {
	t.Helper()
	ctx := context.Background()
	cfg := s.db.Pool.Config()
	rec := &feedRecorder{}
	cfg.ConnConfig.Tracer = rec
	db, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	orig := s.db
	s.db = db
	defer func() { s.db = orig }()
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return w.Code, rec.got
}

// countStatements is how many of got contain every one of parts.
func countStatements(got []feedStatement, parts ...string) int {
	n := 0
next:
	for _, st := range got {
		for _, p := range parts {
			if !strings.Contains(st.sql, p) {
				continue next
			}
		}
		n++
	}
	return n
}

// by=member and by=owner name their owners in a constant number of
// queries, however many buckets the series has: one for people, one for
// service accounts (it was one a row: 7,450 for 31 days hourly).
func TestCostByMemberOwnerQueries(t *testing.T) {
	s, keys := costLabelFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO users(id,email,name) VALUES('usr_cost','cost@example.com','Cost Member'),('usr_two','two@example.com','')`)
	execSQL(t, s, ctx, `INSERT INTO memberships(tenant_id,user_id,role) VALUES('t1','usr_cost','member'),('t1','usr_two','member')`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_user_id='usr_cost' WHERE id='r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_user_id='usr_two' WHERE id='r4'`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_sa_id=(SELECT service_account_id FROM api_keys WHERE id='k1') WHERE id='r3'`)
	count := func(hours int) (people, accounts, series int) {
		// One more bucket an hour, each with every owner.
		execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount, allocated, unallocated)
			SELECT $1::timestamptz + make_interval(hours => h), 't1', r, 'compute', 'compute', 'USD', 'h1', 'blue', 1, 0, 0
			FROM generate_series(1, $2::int) h, unnest(ARRAY['r1','r3','r4']) r ON CONFLICT DO NOTHING`, t0, hours)
		path := "/v1/costs?from=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(time.Duration(hours+1)*time.Hour).Format(time.RFC3339)) + "&by=member&interval=hour"
		_, got := tracedRequest(t, s, keys["t1"], http.MethodGet, path)
		var body CostSummaryBody
		if code := getJSON(t, s, keys["t1"], path, &body); code != 200 {
			t.Fatal(code)
		}
		for _, r := range body.Series {
			if r.Key != "(none)" && r.Owner == nil {
				t.Errorf("series row %s has no owner", r.Key)
			}
		}
		return countStatements(got, "FROM users"), countStatements(got, "FROM service_accounts"), len(body.Series)
	}
	p1, a1, n1 := count(2)
	p2, a2, n2 := count(24)
	if n2 <= n1 {
		t.Fatalf("series did not grow: %d then %d rows", n1, n2)
	}
	if p1 != p2 || a1 != a2 || p2 > 1 || a2 > 1 {
		t.Errorf("owner queries grow with buckets: people %d→%d, accounts %d→%d (series %d→%d rows)", p1, p2, a1, a2, n1, n2)
	}
}

// The service account, people and invitation lists read the people they
// show in one query, not one (or two) a row: the count of statements on
// users and memberships does not grow with the list.
func TestPeopleListsBatchHydration(t *testing.T) {
	f, _ := accessFixture(t)
	ctx := context.Background()
	stmts := func(path string) (users, memberships int) {
		_, got := tracedRequest(t, f.s, f.keys[map[bool]string{true: "op", false: "admin"}[path == "/v1/people"]], http.MethodGet, path)
		return countStatements(got, "FROM users"), countStatements(got, "FROM memberships")
	}
	type counts struct{ users, memberships int }
	before := map[string]counts{}
	paths := []string{"/v1/service-accounts", "/v1/people", "/v1/invitations?status=all"}
	add := func(i int) {
		id := fmt.Sprintf("usr_n%02d", i)
		execSQL(t, f.s, ctx, `INSERT INTO users (id, email, name) VALUES ($1, $1 || '@example.com', 'n')`, id)
		execSQL(t, f.s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', $1, 'member'), ('t2', $1, 'viewer')`, id)
		execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role, owner_user_id, created_by) VALUES ($1, 't1', $2, 'member', $3, 'usr_admin')`,
			fmt.Sprintf("sa_n%02d", i), fmt.Sprintf("bot-%02d", i), id)
		execSQL(t, f.s, ctx, `INSERT INTO invitations (id, tenant_id, code, email, role, invited_by, expires_at, revoked_at, revoked_by)
			VALUES ($1, 't1', $1, $1 || '@example.com', 'member', $2, now() + interval '1 day', now(), 'usr_owner')`, fmt.Sprintf("inv_n%02d", i), id)
	}
	add(0)
	for _, p := range paths {
		u, m := stmts(p)
		before[p] = counts{u, m}
	}
	for i := 1; i <= 12; i++ {
		add(i)
	}
	for _, p := range paths {
		u, m := stmts(p)
		b := before[p]
		if u != b.users || m != b.memberships || u > 2 {
			t.Errorf("%s: statements on users %d→%d, memberships %d→%d with 12 more rows", p, b.users, u, b.memberships, m)
		}
	}
	// And the lists still name them.
	code, out := f.do(t, f.keys["admin"], http.MethodGet, "/v1/service-accounts", nil)
	found := false
	for _, a := range out["serviceAccounts"].([]any) {
		a := a.(map[string]any)
		if a["name"] == "bot-03" {
			found = true
			if a["owner"].(map[string]any)["email"] != "usr_n03@example.com" || a["createdBy"].(map[string]any)["id"] != "usr_admin" {
				t.Errorf("bot-03: %v", a)
			}
		}
	}
	if code != 200 || !found {
		t.Errorf("service accounts: %d %v", code, out)
	}
	_, out = f.do(t, f.keys["op"], http.MethodGet, "/v1/people", nil)
	for _, p := range out["people"].([]any) {
		p := p.(map[string]any)
		if p["id"] == "usr_n05" && len(p["memberships"].([]any)) != 2 {
			t.Errorf("usr_n05 memberships: %v", p["memberships"])
		}
	}
	_, out = f.do(t, f.keys["admin"], http.MethodGet, "/v1/invitations?status=revoked", nil)
	for _, v := range out["invitations"].([]any) {
		v := v.(map[string]any)
		if v["id"] == "inv_n07" && (v["invitedBy"].(map[string]any)["id"] != "usr_n07" || v["revokedBy"].(map[string]any)["id"] != "usr_owner") {
			t.Errorf("inv_n07: %v", v)
		}
	}
}

// owner= and mine=true filter Runs on the owner columns against ids
// resolved first: no name subquery ORed into the Runs query (which made
// the planner scan every Run instead of using runs_owner_user/_sa).
func TestRunOwnerFilterUsesOwnerIDs(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	for _, tc := range []struct {
		key, q string
		want   []string // the predicates the Runs query has
		absent []string
	}{
		{"viewer", "owner=mia@example.com", []string{"r.owner_user_id = ANY("}, []string{"r.owner_sa_id = ANY("}},
		{"viewer", "owner=sa-bot", []string{"r.owner_sa_id = ANY("}, []string{"r.owner_user_id = ANY("}},
		{"viewer", "owner=user:usr_owner", []string{"r.owner_user_id = ANY("}, []string{"r.owner_sa_id = ANY("}},
		{"viewer", "owner=nobody@example.com", []string{"AND false"}, []string{"r.owner_user_id = ANY(", "r.owner_sa_id = ANY("}},
		{"member", "mine=true", []string{"r.owner_user_id = ANY(", "r.owner_sa_id = ANY("}, nil},
		{"member", "mine=true&sort=id", []string{"r.owner_user_id = ANY(", "r.owner_sa_id = ANY("}, nil},
		{"sa", "mine=true", []string{"r.owner_sa_id = ANY("}, []string{"r.owner_user_id = ANY("}},
	} {
		_, got := tracedRequest(t, f.s, f.keys[tc.key], http.MethodGet, "/v1/runs?"+tc.q)
		var runsQ []string
		for _, st := range got {
			if strings.Contains(st.sql, "FROM runs r WHERE") || strings.Contains(st.sql, "runs r") && strings.Contains(st.sql, "WHERE true") {
				runsQ = append(runsQ, st.sql)
			}
		}
		if len(runsQ) == 0 {
			t.Fatalf("%s: no Runs query among %d statements", tc.q, len(got))
		}
		for _, sql := range runsQ {
			for _, w := range tc.want {
				if !strings.Contains(sql, w) {
					t.Errorf("%s: Runs query lacks %q:\n%s", tc.q, w, sql)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(sql, a) {
					t.Errorf("%s: Runs query has %q:\n%s", tc.q, a, sql)
				}
			}
			for _, bad := range []string{"IN (SELECT u.id FROM users", "IN (SELECT sa.id FROM service_accounts"} {
				if strings.Contains(sql, bad) {
					t.Errorf("%s: Runs query resolves names inline (%s):\n%s", tc.q, bad, sql)
				}
			}
		}
	}
}
