package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// accessFixture: peopleFixture behind Cloudflare Access, with the operator
// op@example.com and default tenant t1.
func accessFixture(t *testing.T) (*peopleFix, fakeAccess) {
	t.Helper()
	f := peopleFixture(t)
	access := newFakeAccess(t, "console-aud")
	f.s.cfg.ConsoleAuth = ConsoleAuth{Mode: "cloudflare-access", CFTeam: access.URL, CFAud: "console-aud",
		CFOperators: []string{"op@example.com"}, CFDefaultTenant: "t1"}
	f.s.cfAccess = newCFAccess(access.URL, "console-aud")
	f.s.cfAccess.client = &http.Client{Transport: failingTransport{}}
	if err := f.s.initCFTenant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.syncPeople(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f, access
}

// failingTransport: Access's identity endpoint is not there; names are emails.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, http.ErrHandlerTimeout }

func accessDo(t *testing.T, s *Server, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	w := accessCall(t, s, token, method, path, body)
	return w.code, w.out
}

type accessAnswer struct {
	code int
	out  map[string]any
}

func accessCall(t *testing.T, s *Server, token, method, path string, body any) accessAnswer {
	t.Helper()
	req := httptest.NewRequest(method, path, jsonBody(t, body))
	req.Header.Set("Cf-Access-Jwt-Assertion", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return accessAnswer{w.Code, decodeMap(w.Body.Bytes())}
}

// Signed in through Access: an operator is one; a member acts with their
// role in their tenant; anyone else is 403 not_a_member, but for their own
// profile and invitations. Nobody is an admin of the default tenant just
// for being signed in.
func TestAccessMembership(t *testing.T) {
	f, access := accessFixture(t)
	s := f.s
	code, who := accessDo(t, s, access.sign("op@example.com"), http.MethodGet, "/v1/whoami", nil)
	if code != 200 || who["operator"] != true {
		t.Fatalf("operator: %d %v", code, who)
	}
	code, who = accessDo(t, s, access.sign("MIA@example.com"), http.MethodGet, "/v1/whoami", nil)
	if code != 200 || who["role"] != "member" || who["tenantId"] != "t1" || who["userId"] != "usr_member" {
		t.Fatalf("member: %d %v", code, who)
	}
	code, out := accessDo(t, s, access.sign("stranger@example.com"), http.MethodGet, "/v1/runs", nil)
	if c, _ := errCode(out); code != http.StatusForbidden || c != "not_a_member" {
		t.Fatalf("stranger lists Runs: %d %v", code, out)
	}
	code, out = accessDo(t, s, access.sign("stranger@example.com"), http.MethodGet, "/v1/whoami", nil)
	if c, _ := errCode(out); code != http.StatusForbidden || c != "not_a_member" {
		t.Fatalf("stranger whoami: %d %v", code, out)
	}
	code, out = accessDo(t, s, access.sign("stranger@example.com"), http.MethodGet, "/v1/me", nil)
	if code != 200 || out["email"] != "stranger@example.com" {
		t.Fatalf("stranger's profile: %d %v", code, out)
	}
	// A member of t1 asking for t2: not a member there.
	code, out = accessDo(t, s, access.sign("mia@example.com"), http.MethodGet, "/v1/runs?tenant=t2", nil)
	if c, _ := errCode(out); code != http.StatusForbidden || c != "not_a_member" {
		t.Fatalf("member asking for another tenant: %d %v", code, out)
	}
	// In two tenants: ?tenant= picks; the default tenant otherwise.
	execSQL(t, s, context.Background(), `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t2', 'usr_member', 'admin')`)
	code, who = accessDo(t, s, access.sign("mia@example.com"), http.MethodGet, "/v1/whoami?tenant=t2", nil)
	if code != 200 || who["role"] != "admin" || who["tenantId"] != "t2" || len(who["memberships"].([]any)) != 2 {
		t.Fatalf("member picking t2: %d %v", code, who)
	}
	code, who = accessDo(t, s, access.sign("mia@example.com"), http.MethodGet, "/v1/whoami", nil)
	if code != 200 || who["tenantId"] != "t1" {
		t.Fatalf("member by default: %d %v", code, who)
	}
	// A viewer reads but submits nothing.
	if code, _ := accessDo(t, s, access.sign("vic@example.com"), http.MethodGet, "/v1/runs", nil); code != 200 {
		t.Fatalf("viewer reads: %d", code)
	}
	if code, out := accessDo(t, s, access.sign("vic@example.com"), http.MethodPost, "/v1/runs", rawJSON(minimalRun)); code != http.StatusForbidden {
		t.Fatalf("viewer submits: %d %v", code, out)
	}
	// A person signed in owns what they submit.
	code, out = accessDo(t, s, access.sign("mia@example.com"), http.MethodPost, "/v1/runs", rawJSON(minimalRun))
	if code != http.StatusCreated {
		t.Fatalf("member submits: %d %v", code, out)
	}
	if o := queryOne[*string](t, s, `SELECT owner_user_id FROM runs WHERE id = $1`, out["id"]); o == nil || *o != "usr_member" {
		t.Fatalf("owner: %v", o)
	}
}

// Previews need no membership (decision 13): any verified Access user may
// open the default tenant's, a member may open their tenant's, an operator
// any; anyone else none.
func TestAccessPreviewMembership(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"},
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	access := newFakeAccess(t, "preview-aud")
	s.cfg.ConsoleAuth = ConsoleAuth{Mode: "cloudflare-access", CFTeam: access.URL, CFOperators: []string{"op@example.com"}}
	s.cfg.Preview.Auth, s.cfg.Preview.CFAud = "cloudflare-access", "preview-aud"
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO users (id, email) VALUES ('usr_m', 'member@example.com')`)
	execSQL(t, s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', 'usr_m', 'viewer')`)
	get := func(email string) int {
		req := httptest.NewRequest(http.MethodGet, "https://web.pr1.lux.example.com/", nil)
		req.Header.Set("Cf-Access-Jwt-Assertion", access.sign(email))
		w := httptest.NewRecorder()
		s.preview.ServeHTTP(w, req)
		return w.Code
	}
	_ = sv
	// The server is t1's. Default tenant t1: a stranger opens it.
	s.cfg.ConsoleAuth.CFDefaultTenant, s.cfTenantID = "t1", "t1"
	if c := get("stranger@example.com"); c == http.StatusUnauthorized {
		t.Errorf("non-member, default tenant: %d", c)
	}
	// Default tenant t2: the stranger does not; a member and an operator do.
	s.cfg.ConsoleAuth.CFDefaultTenant, s.cfTenantID = "t2", "t2"
	if c := get("stranger@example.com"); c != http.StatusUnauthorized {
		t.Errorf("non-member, another tenant: %d", c)
	}
	if c := get("member@example.com"); c == http.StatusUnauthorized {
		t.Errorf("member: %d", c)
	}
	if c := get("op@example.com"); c == http.StatusUnauthorized {
		t.Errorf("operator: %d", c)
	}
}
