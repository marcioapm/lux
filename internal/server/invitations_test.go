package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestInvitations(t *testing.T) {
	f, access := accessFixture(t)
	s := f.s
	invite := func(email, role string) (int, map[string]any) {
		return f.do(t, f.keys["admin"], "POST", "/v1/invitations", map[string]any{"role": role, "invitations": []any{map[string]any{"email": email}}})
	}
	code, out := invite("new@example.com", "member")
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	result := func(out map[string]any) map[string]any { return out["results"].([]any)[0].(map[string]any) }
	r := result(out)
	if r["result"] != "created" {
		t.Fatal(r)
	}
	v := r["invitation"].(map[string]any)
	id, invcode := v["id"].(string), v["code"].(string)
	_, out = invite("NEW@example.com", "member")
	if result(out)["result"] != "existing" || result(out)["invitation"].(map[string]any)["code"] != invcode {
		t.Fatal(out)
	}
	execSQL(t, s, context.Background(), `UPDATE invitations SET expires_at=now()-interval '1 hour' WHERE id=$1`, id)
	_, out = invite("new@example.com", "member")
	if result(out)["result"] != "renewed" || result(out)["invitation"].(map[string]any)["code"] != invcode {
		t.Fatal(out)
	}
	_, out = invite("mia@example.com", "member")
	if result(out)["result"] != "member" {
		t.Fatal(out)
	}
	_, out = invite("not-an-email", "member")
	if result(out)["result"] != "invalid" {
		t.Fatal(out)
	}
	if code, _ := invite("boss@example.com", "owner"); code != 403 {
		t.Error(code)
	}
	wrong := access.sign("wrong@example.com")
	right := access.sign("NEW@example.com")
	code, out = accessDo(t, s, wrong, "GET", "/v1/invitations/by-code/"+invcode, nil)
	if code != 200 || out["emailMatches"] != false || len(out) != 2 {
		t.Fatalf("wrong email lookup leaks: %d %v", code, out)
	}
	code, out = accessDo(t, s, wrong, "POST", "/v1/invitations/by-code/"+invcode+"/accept", nil)
	if c, _ := errCode(out); code != 403 || c != "invitation_email_mismatch" {
		t.Fatalf("wrong accept: %d %v", code, out)
	}
	code, out = accessDo(t, s, right, "GET", "/v1/invitations/by-code/"+invcode, nil)
	if code != 200 || out["emailMatches"] != true || out["tenant"] != "t1" {
		t.Fatal(code, out)
	}
	code, out = accessDo(t, s, right, "GET", "/v1/me/invitations", nil)
	if code != 200 || len(out["invitations"].([]any)) != 1 {
		t.Fatal(code, out)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := accessDo(t, s, right, "POST", "/v1/invitations/by-code/"+invcode+"/accept", nil)
			mu.Lock()
			counts[code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counts[200] != 1 || counts[410] != 1 {
		t.Fatalf("concurrent accepts: %v", counts)
	}
	if n := queryOne[int64](t, s, `SELECT count(*) FROM memberships m JOIN users u ON u.id=m.user_id WHERE lower(u.email)='new@example.com'`); n != 1 {
		t.Fatal(n)
	}
	for _, status := range []string{"expired", "revoked"} {
		email := status + "@example.com"
		_, out = invite(email, "viewer")
		v := result(out)["invitation"].(map[string]any)
		id := v["id"].(string)
		c := v["code"].(string)
		if status == "expired" {
			execSQL(t, s, context.Background(), `UPDATE invitations SET expires_at=now()-interval '1 hour' WHERE id=$1`, id)
		} else {
			if code, out := f.do(t, f.keys["admin"], "POST", "/v1/invitations/"+id+"/revoke", nil); code != 200 {
				t.Fatal(code, out)
			}
		}
		code, out := accessDo(t, s, access.sign(email), "POST", "/v1/invitations/by-code/"+c+"/accept", nil)
		if e, _ := errCode(out); code != 410 || e != "invitation_unusable" {
			t.Errorf("%s: %d %v", status, code, out)
		}
	}
	// Every invitation route refuses key console mode before doing anything.
	s.cfg.ConsoleAuth.Mode = "key"
	for _, rt := range []struct {
		method, path string
		body         any
	}{{"GET", "/v1/invitations", nil}, {"GET", "/v1/invitations/counts", nil}, {"POST", "/v1/invitations", map[string]any{"role": "member", "invitations": []any{}}}, {"POST", "/v1/invitations/" + id + "/renew", nil}, {"POST", "/v1/invitations/" + id + "/revoke", nil}, {"GET", "/v1/invitations/by-code/" + invcode, nil}, {"POST", "/v1/invitations/by-code/" + invcode + "/accept", nil}, {"GET", "/v1/me/invitations", nil}} {
		code, out := f.do(t, f.keys["admin"], rt.method, rt.path, rt.body)
		if c, _ := errCode(out); code != 409 || c != "invitations_unavailable" {
			t.Errorf("%s: %d %v", rt.path, code, out)
		}
	}
	_ = fmt.Sprint
}

// Renew and revoke by email act on the live invitation, never a spent one
// of the same email (revoked or accepted, whatever their ids sort as); by
// id, a spent invitation is still 410.
func TestInvitationByEmailIsTheLiveOne(t *testing.T) {
	f, _ := accessFixture(t)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO invitations (id, tenant_id, code, email, role, expires_at, revoked_at, accepted_at) VALUES
		('inv_0000_revoked', 't1', 'code-revoked', 'a@example.com', 'member', now() + interval '1 day', now(), NULL),
		('inv_0001_accepted', 't1', 'code-accepted', 'a@example.com', 'member', now() + interval '1 day', NULL, now()),
		('inv_zzzz_live', 't1', 'code-live', 'a@example.com', 'member', now() + interval '1 day', NULL, NULL)`)
	for _, action := range []string{"renew", "revoke"} {
		code, out := f.do(t, f.keys["admin"], "POST", "/v1/invitations/A@example.com/"+action, nil)
		if code != 200 || out["id"] != "inv_zzzz_live" {
			t.Fatalf("%s by email: %d %v, want 200 on inv_zzzz_live", action, code, out)
		}
	}
	if got := queryOne[bool](t, f.s, `SELECT revoked_at IS NOT NULL AND renewed_at IS NOT NULL FROM invitations WHERE id = 'inv_zzzz_live'`); !got {
		t.Error("the live invitation was not renewed and revoked")
	}
	code, out := f.do(t, f.keys["admin"], "POST", "/v1/invitations/inv_0000_revoked/renew", nil)
	if c, _ := errCode(out); code != 410 || c != "invitation_unusable" {
		t.Errorf("renew a revoked one by id: %d %v", code, out)
	}
	// None live now: by email, nothing to act on.
	if code, out := f.do(t, f.keys["admin"], "POST", "/v1/invitations/a@example.com/revoke", nil); code != 404 {
		t.Errorf("revoke by email with none live: %d %v", code, out)
	}
}
