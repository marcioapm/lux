package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
