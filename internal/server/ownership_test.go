package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// ownedRuns adds t1 Runs owned by the member (r_mia), the owner (r_olive),
// the member's service account (r_sa), and no one (r_none).
func ownedRuns(t *testing.T, f *peopleFix) {
	t.Helper()
	execSQL(t, f.s, context.Background(), `INSERT INTO runs (id, tenant_id, spec, state, owner_user_id, owner_sa_id) VALUES
		('r_mia', 't1', '{}', 'stopped', 'usr_member', NULL), ('r_olive', 't1', '{}', 'stopped', 'usr_owner', NULL),
		('r_sa', 't1', '{}', 'stopped', NULL, 'sa_bot'), ('r_none', 't1', '{}', 'stopped', NULL, NULL)`)
}

func rename(t *testing.T, f *peopleFix, key, run string) (int, map[string]any) {
	t.Helper()
	return f.do(t, key, http.MethodPatch, "/v1/runs/"+run, map[string]any{"name": "renamed-" + run})
}

// Who may act on a Run: members their own and their service accounts',
// admins and owners every Run, viewers none; a refusal names the owner.
func TestRunOwnership(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	type want struct{ mia, olive, sa, none int }
	ok, no := http.StatusOK, http.StatusForbidden
	for _, tc := range []struct {
		key string
		want
	}{
		{"owner", want{ok, ok, ok, ok}},
		{"admin", want{ok, ok, ok, ok}},
		{"member", want{ok, no, ok, no}},  // their own, and their service account's
		{"member2", want{no, no, no, no}}, // another member
		{"viewer", want{no, no, no, no}},
		{"sa", want{no, no, ok, no}}, // the account's own
		{"op", want{ok, ok, ok, ok}},
	} {
		for run, code := range map[string]int{"r_mia": tc.mia, "r_olive": tc.olive, "r_sa": tc.sa, "r_none": tc.none} {
			got, out := rename(t, f, f.keys[tc.key], run)
			if got != code {
				t.Errorf("%s renames %s: %d %v, want %d", tc.key, run, got, out, code)
				continue
			}
			if got == no && tc.key != "viewer" {
				if c, msg := errCode(out); c != "not_run_owner" {
					t.Errorf("%s on %s: code %s %s", tc.key, run, c, msg)
				} else if run == "r_olive" && !strings.Contains(msg, "olive@example.com") {
					t.Errorf("message names no owner: %s", msg)
				}
			}
		}
	}
}

// A server moved to another person's Run while a change to it waits on its
// row is judged by the Run it is attached to now: the member who could act
// on the Run it was on when the request came in gets 403, and the server
// is untouched. Deterministic: the move holds the server's row (as an
// admin's detach + attach would) until the request is blocked on it.
func TestServerChangeRechecksMovedServer(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO run_servers (id, host, tenant_id, run_id, name, port, state, labels)
		VALUES ('srv_mmmmmmmmmmmmmmmm', 'web-mmmmmmmm', 't1', 'r_mia', 'web', 3000, 'stopped', '{}')`)
	for _, rt := range []struct{ method, path string }{
		{http.MethodPatch, "/v1/servers/srv_mmmmmmmmmmmmmmmm"},
		{http.MethodPost, "/v1/servers/srv_mmmmmmmmmmmmmmmm/detach"},
		{http.MethodDelete, "/v1/servers/srv_mmmmmmmmmmmmmmmm"},
	} {
		execSQL(t, f.s, ctx, `UPDATE run_servers SET run_id = 'r_mia', labels = '{}' WHERE id = 'srv_mmmmmmmmmmmmmmmm'`)
		locked, release := make(chan struct{}), make(chan struct{})
		moved := make(chan error, 1)
		go func() {
			moved <- f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE run_servers SET run_id = 'r_olive' WHERE id = 'srv_mmmmmmmmmmmmmmmm'`); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		<-locked
		type res struct {
			code int
			out  map[string]any
		}
		done := make(chan res, 1)
		go func() {
			var body any
			if rt.method == http.MethodPatch {
				body = map[string]any{"labels": map[string]string{"hijacked": "yes"}}
			}
			code, out := f.do(t, f.keys["member"], rt.method, rt.path, body)
			done <- res{code, out}
		}()
		if err := waitLocked(f.s, 1, nil); err != nil { // the request, on the server's row
			close(release)
			t.Fatal(err)
		}
		close(release)
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		r := <-done
		if c, _ := errCode(r.out); r.code != http.StatusForbidden || c != "not_run_owner" {
			t.Errorf("%s %s on a server moved to olive's Run: %d %v, want 403 not_run_owner", rt.method, rt.path, r.code, r.out)
		}
		if got := queryOne[string](t, f.s, `SELECT coalesce(run_id, '') || ' ' || labels::text FROM run_servers WHERE id = 'srv_mmmmmmmmmmmmmmmm'`); got != "r_olive {}" {
			t.Errorf("%s %s: server changed: %q", rt.method, rt.path, got)
		}
	}
	// A server says its tenant: the console decides who may change it there.
	if code, out := f.do(t, f.keys["viewer"], http.MethodGet, "/v1/servers/srv_mmmmmmmmmmmmmmmm", nil); code != 200 || out["tenantId"] != "t1" {
		t.Errorf("server tenantId: %d %v", code, out["tenantId"])
	}
}

// Every Run-changing route checks the owner, not just rename: a member of
// the tenant gets not_run_owner on another's Run for each.
func TestRunOwnershipEveryRoute(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	key := f.keys["member2"]
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/v1/runs/r_mia", map[string]any{"name": "x"}},
		{http.MethodPost, "/v1/runs/r_mia/timeout", map[string]any{"unlimited": true}},
		{http.MethodPost, "/v1/runs/r_mia/stop", nil},
		{http.MethodPost, "/v1/runs/r_mia/resume", map[string]any{}},
		{http.MethodPost, "/v1/runs/r_mia/terminate", nil},
		{http.MethodPost, "/v1/runs/r_mia/cancel", nil},
		{http.MethodPost, "/v1/runs/r_mia/push", map[string]any{}},
		{http.MethodPost, "/v1/runs/r_mia/sync", map[string]any{}},
		{http.MethodPost, "/v1/runs/r_mia/checkpoint", nil},
		{http.MethodDelete, "/v1/runs/r_mia/artifacts", nil},
		{http.MethodPost, "/v1/runs/r_mia/input", map[string]any{"text": "hi"}},
		{http.MethodGet, "/v1/runs/r_mia/exec", nil},
		{http.MethodGet, "/v1/runs/r_mia/attach", nil},
		{http.MethodGet, "/v1/runs/r_mia/ports/web", nil},
		{http.MethodPost, "/v1/runs/r_mia/servers", map[string]any{"name": "web", "port": 3000}},
		{http.MethodPut, "/v1/runs/r_mia/servers/web", map[string]any{"port": 3000}},
		{http.MethodPost, "/v1/runs/r_mia/servers/web/start", nil},
		{http.MethodPost, "/v1/runs/r_mia/servers/web/stop", nil},
		{http.MethodPost, "/v1/runs/r_mia/servers/web/restart", nil},
		{http.MethodDelete, "/v1/runs/r_mia/servers/web", nil},
		{http.MethodPost, "/v1/runs/r_mia/tickets", map[string]any{"kind": "exec"}},
		{http.MethodPost, "/v1/servers", map[string]any{"name": "web2", "port": 3000, "runId": "r_mia"}},
	} {
		code, out := f.do(t, key, rt.method, rt.path, rt.body)
		if c, _ := errCode(out); code != http.StatusForbidden || c != "not_run_owner" {
			t.Errorf("%s %s: %d %v", rt.method, rt.path, code, out)
		}
	}
	// The owner gets past the check on the same routes (whatever the Run's
	// state makes of them then).
	code, out := f.do(t, f.keys["member"], http.MethodPost, "/v1/runs/r_mia/stop", nil)
	if c, _ := errCode(out); c == "not_run_owner" {
		t.Errorf("owner stops: %d %v", code, out)
	}
}

// A removed member's Run stays theirs: listed with them as owner.
func TestRunOwnerShownAndFiltered(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	execSQL(t, f.s, context.Background(), `DELETE FROM memberships WHERE user_id = 'usr_member'`)
	var list struct{ Runs []Run }
	if code := getJSON(t, f.s, f.keys["admin"], "/v1/runs", &list); code != 200 {
		t.Fatal(code)
	}
	owners := map[string]string{}
	for _, r := range list.Runs {
		if r.Owner != nil {
			owners[r.ID] = r.Owner.Kind + ":" + r.Owner.ID + ":" + r.Owner.Email
		} else {
			owners[r.ID] = ""
		}
	}
	for id, w := range map[string]string{"r_mia": "user:usr_member:mia@example.com", "r_olive": "user:usr_owner:olive@example.com", "r_sa": "serviceAccount:sa_bot:", "r_none": ""} {
		if owners[id] != w {
			t.Errorf("%s owner %q, want %q", id, owners[id], w)
		}
	}
	ids := func(key, q string) string {
		var l struct{ Runs []Run }
		if code := getJSON(t, f.s, key, "/v1/runs?"+q, &l); code != 200 {
			t.Fatalf("%s: %d", q, code)
		}
		var out []string
		for _, r := range l.Runs {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	execSQL(t, f.s, context.Background(), `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t1', 'usr_member', 'member')`)
	for q, w := range map[string]string{"owner=mia@example.com": "r_mia", "owner=MIA@example.com": "r_mia", "owner=sa-bot": "r_sa", "owner=usr_owner": "r_olive",
		// Several owners: any of them; user:/sa: exactly (a member's page: them and their accounts).
		"owner=user:usr_member,sa:sa_bot&sort=id": "r_mia,r_sa", "owner=mia@example.com,sa-bot&sort=id": "r_mia,r_sa",
		"owner=user:usr_owner": "r_olive", "owner=sa:sa_bot": "r_sa", "owner=user:mia@example.com": "", "owner=sa:sa-bot": "", "owner=user:nobody,sa:nothing": ""} {
		if got := ids(f.keys["viewer"], q); got != w {
			t.Errorf("%s: %s, want %s", q, got, w)
		}
	}
	if got := ids(f.keys["member"], "mine=true&sort=id"); got != "r_mia,r_sa" {
		t.Errorf("member mine: %s", got)
	}
	if got := ids(f.keys["sa"], "mine=true"); got != "r_sa" {
		t.Errorf("account mine: %s", got)
	}
}

// Submitting records the owner: the person by their key, the account by its.
func TestRunOwnerAtSubmit(t *testing.T) {
	f := peopleFixture(t)
	for key, want := range map[string]string{"member": "user:usr_member", "sa": "sa:sa_bot", "op": ""} {
		path := "/v1/runs"
		if key == "op" {
			path += "?tenant=t1"
		}
		code, out := postJSON(t, f.s, f.keys[key], path, minimalRun)
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %v", key, code, out)
		}
		got := queryOne[string](t, f.s, `SELECT coalesce('user:' || owner_user_id, 'sa:' || owner_sa_id, '') FROM runs WHERE id = $1`, out["id"])
		if got != want {
			t.Errorf("%s: owner %q, want %q", key, got, want)
		}
	}
}
