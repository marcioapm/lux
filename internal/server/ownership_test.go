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
func TestStandaloneServerLifecycle(t *testing.T) {
	for _, actor := range []string{"member", "sa"} {
		t.Run(actor, func(t *testing.T) {
			f := peopleFixture(t)
			ownedRuns(t, f)
			run := "r_mia"
			if actor == "sa" {
				run = "r_sa"
			}
			execSQL(t, f.s, context.Background(), `INSERT INTO runs (id, tenant_id, spec, state, owner_user_id, owner_sa_id) SELECT 'r_second', tenant_id, spec, state, owner_user_id, owner_sa_id FROM runs WHERE id = $1`, run)
			code, out := f.do(t, f.keys[actor], http.MethodPost, "/v1/servers", map[string]any{"name": "preview", "port": 3000, "wake": "request", "lifetime": "owner"})
			if code != http.StatusCreated || out["state"] != "asleep" {
				t.Fatalf("Dude shape create: %d %v", code, out)
			}
			id := out["id"].(string)
			owner := out["owner"]
			path := "/v1/servers/" + id
			for _, step := range []struct {
				method, suffix string
				body           any
				status         int
				runID          any
			}{
				{http.MethodPatch, "", map[string]any{"labels": map[string]string{"preview": "yes"}}, 200, nil},
				{http.MethodPost, "/stop", nil, 200, nil},
				{http.MethodPost, "/start", nil, 409, nil},
				{http.MethodPost, "/restart", nil, 409, nil},
				{http.MethodPost, "/attach", map[string]any{"runId": run}, 200, run},
				{http.MethodPost, "/detach", nil, 200, nil},
				{http.MethodPost, "/attach", map[string]any{"runId": "r_second"}, 200, "r_second"},
				{http.MethodDelete, "", nil, 204, nil},
			} {
				code, out = f.do(t, f.keys[actor], step.method, path+step.suffix, step.body)
				if code != step.status {
					t.Fatalf("%s %s: %d %v", step.method, step.suffix, code, out)
				}
				if step.status == 409 {
					if c, _ := errCode(out); c != "not_attached" {
						t.Fatalf("expected exact business error: %v", out)
					}
				} else if step.method != http.MethodDelete && out["owner"] != owner {
					t.Fatalf("creator changed: %v", out)
				}
				if step.method == http.MethodDelete {
					if code, out := f.do(t, f.keys[actor], http.MethodGet, path, nil); code != http.StatusNotFound {
						t.Fatalf("deleted server GET: %d %v", code, out)
					}
					if queryOne[bool](t, f.s, `SELECT EXISTS (SELECT 1 FROM run_servers WHERE id=$1)`, id) {
						t.Fatal("deleted server still stored")
					}
				} else if step.status == http.StatusOK {
					if out["runId"] != step.runID {
						t.Fatalf("%s response runId: %v, want %v", step.suffix, out["runId"], step.runID)
					}
					stored := queryOne[*string](t, f.s, `SELECT run_id FROM run_servers WHERE id=$1`, id)
					if (stored == nil) != (step.runID == nil) || stored != nil && *stored != step.runID {
						t.Fatalf("%s stored runId: %v, want %v", step.suffix, stored, step.runID)
					}
				}
			}
		})
	}
}

func TestStandaloneServerPrincipals(t *testing.T) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	ctx := context.Background()
	f.addKey(t, "sa_second", "service_account_id", "sa_bot")
	f.addKey(t, "personal_second", "user_id", "usr_member")
	f.addKey(t, "readonly", "user_id", "usr_admin")
	execSQL(t, f.s, ctx, `UPDATE api_keys SET read_only = true WHERE id = 'k_readonly'`)
	execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role, owner_user_id) VALUES ('sa_other', 't1', 'other', 'member', 'usr_member2')`)
	f.addKey(t, "other_sa", "service_account_id", "sa_other")
	execSQL(t, f.s, ctx, `INSERT INTO service_accounts (id, tenant_id, name, role, owner_user_id) VALUES ('sa_sibling', 't1', 'sibling', 'member', 'usr_member')`)
	f.addKey(t, "sibling_sa", "service_account_id", "sa_sibling")
	execSQL(t, f.s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, owner_user_id, owner_sa_id) VALUES
		('r_member2', 't1', '{}', 'stopped', 'usr_member2', NULL),
		('r_sibling', 't1', '{}', 'stopped', NULL, 'sa_sibling')`)
	f.addKey(t, "foreign", "user_id", "usr_member")
	execSQL(t, f.s, ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ('t2','usr_member','member')`)
	execSQL(t, f.s, ctx, `UPDATE api_keys SET tenant_id = 't2' WHERE id = 'k_foreign'`)
	// Existing legacy records, no rewrite or new ownership metadata needed.
	for id, actor := range map[string]string{"srv_email": "mia@example.com", "srv_key": "k_sa", "srv_personal": "k_member", "srv_unknown": "lost-actor", "srv_platform": "k_op"} {
		execSQL(t, f.s, ctx, `INSERT INTO run_servers (id, tenant_id, name, port, owner, wake, lifetime) VALUES ($1,'t1','web',3000,$2,'request','owner')`, id, actor)
	}
	patch := map[string]any{"labels": map[string]string{"changed": "yes"}}
	// Each attacker owns the destination Run, isolating source authorization.
	for _, attack := range []struct{ key, server, run string }{
		{"sibling_sa", "srv_key", "r_sibling"},
		{"member2", "srv_email", "r_member2"},
	} {
		for _, route := range []struct {
			method, suffix string
			body           any
		}{
			{http.MethodPatch, "", patch}, {http.MethodDelete, "", nil},
			{http.MethodPost, "/attach", map[string]any{"runId": attack.run}},
		} {
			before := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id=$1`, attack.server)
			if code, out := f.do(t, f.keys[attack.key], route.method, "/v1/servers/"+attack.server+route.suffix, route.body); code != http.StatusForbidden {
				t.Fatalf("%s %s %s: %d %v", attack.key, route.method, attack.server+route.suffix, code, out)
			}
			if after := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id=$1`, attack.server); after != before {
				t.Fatalf("denied source request changed row: %s", attack.server)
			}
		}
	}
	execSQL(t, f.s, ctx, `INSERT INTO run_servers (id, tenant_id, name, port, owner, wake, lifetime) VALUES ('srv_sibling','t1','web',3000,'k_sibling_sa','request','owner')`)
	if code, out := f.do(t, f.keys["sa"], http.MethodPatch, "/v1/servers/srv_sibling", patch); code != http.StatusForbidden {
		t.Fatalf("exact SA manages sibling: %d %v", code, out)
	}
	if code, out := f.do(t, f.keys["member"], http.MethodPatch, "/v1/servers/srv_sibling", patch); code != http.StatusOK {
		t.Fatalf("owning person manages sibling: %d %v", code, out)
	}
	for _, id := range []string{"srv_email", "srv_key", "srv_personal"} {
		path := "/v1/servers/" + id
		for _, key := range []string{"member2", "other_sa", "viewer", "readonly", "foreign"} {
			for _, route := range []struct {
				method, suffix string
				body           any
			}{
				{http.MethodPatch, "", patch}, {http.MethodDelete, "", nil},
				{http.MethodPost, "/start", nil}, {http.MethodPost, "/stop", nil}, {http.MethodPost, "/restart", nil},
				{http.MethodPost, "/detach", nil}, {http.MethodPost, "/attach", map[string]any{"runId": "r_mia"}},
			} {
				before := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id=$1`, id)
				code, out := f.do(t, f.keys[key], route.method, path+route.suffix, route.body)
				want := 403
				if key == "foreign" {
					want = 404
				}
				if code != want {
					t.Errorf("%s %s %s: %d %v want %d", key, route.method, path+route.suffix, code, out, want)
				}
				if after := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id=$1`, id); after != before {
					t.Fatalf("denied request changed row: %s", id)
				}
			}
		}
	}
	for _, key := range []string{"viewer", "readonly"} {
		if code, out := f.do(t, f.keys[key], http.MethodPost, "/v1/servers", map[string]any{"name": "web", "port": 3000, "wake": "request", "lifetime": "owner"}); code != 403 {
			t.Errorf("%s creates: %d %v", key, code, out)
		}
	}
	// Same personal identity, whether attribution is email or an old key ID.
	for _, id := range []string{"srv_email", "srv_personal"} {
		for _, key := range []string{"member", "personal_second"} {
			if code, out := f.do(t, f.keys[key], http.MethodPatch, "/v1/servers/"+id, patch); code != 200 {
				t.Errorf("%s manages %s: %d %v", key, id, code, out)
			}
		}
	}
	// Revocation/rotation and rename do not erase attribution. The owning
	// member may manage the SA's server, but another SA of that member may not.
	execSQL(t, f.s, ctx, `UPDATE api_keys SET revoked_at=now() WHERE id='k_sa'`)
	execSQL(t, f.s, ctx, `UPDATE service_accounts SET name='renamed' WHERE id='sa_bot'`)
	for _, key := range []string{"sa_second", "member", "admin", "op"} {
		if code, out := f.do(t, f.keys[key], http.MethodPatch, "/v1/servers/srv_key", patch); code != 200 {
			t.Errorf("%s manages rotated SA server: %d %v", key, code, out)
		}
	}
	// Destination authorization is independent of creator authorization.
	if code, out := f.do(t, f.keys["member"], http.MethodPost, "/v1/servers/srv_email/attach", map[string]any{"runId": "r_olive"}); code != 403 {
		t.Fatalf("creator attaches to other's Run: %d %v", code, out)
	}
	if got := queryOne[string](t, f.s, `SELECT coalesce(run_id,'') FROM run_servers WHERE id='srv_email'`); got != "" {
		t.Fatal("unauthorized attach changed server")
	}
	creator := queryOne[string](t, f.s, `SELECT owner FROM run_servers WHERE id='srv_email'`)
	if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/servers/srv_email/attach", map[string]any{"runId": "r_member2"}); code != 200 {
		t.Fatal(code, out)
	}
	for _, route := range []struct {
		method, suffix string
		body           any
	}{
		{http.MethodPatch, "", patch}, {http.MethodDelete, "", nil}, {http.MethodPost, "/detach", nil},
		{http.MethodPost, "/start", nil}, {http.MethodPost, "/stop", nil}, {http.MethodPost, "/restart", nil},
		{http.MethodPost, "/attach", map[string]any{"runId": "r_mia"}},
	} {
		before := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id='srv_email'`)
		if code, out := f.do(t, f.keys["member"], route.method, "/v1/servers/srv_email"+route.suffix, route.body); code != 403 {
			t.Errorf("creator controls other's attached Run: %d %v", code, out)
		}
		if after := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id='srv_email'`); after != before {
			t.Fatal("attached row changed")
		}
	}
	// Detach by the current Run owner does not transfer creation attribution.
	if code, out := f.do(t, f.keys["member2"], http.MethodPost, "/v1/servers/srv_email/detach", nil); code != 200 || out["owner"] != creator || out["runId"] != nil {
		t.Fatal(code, out)
	}
	before := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id='srv_email'`)
	if code, out := f.do(t, f.keys["member2"], http.MethodPatch, "/v1/servers/srv_email", patch); code != http.StatusForbidden {
		t.Fatalf("detacher owns unattached server: %d %v", code, out)
	}
	if after := queryOne[string](t, f.s, `SELECT row_to_json(s)::text FROM run_servers s WHERE id='srv_email'`); after != before {
		t.Fatal("denied detacher changed row")
	}
	if got := queryOne[string](t, f.s, `SELECT owner FROM run_servers WHERE id='srv_email'`); got != creator {
		t.Fatalf("detach rewrote creator: %s", got)
	}
	if code, out := f.do(t, f.keys["member"], http.MethodPatch, "/v1/servers/srv_email", patch); code != 200 {
		t.Fatal(code, out)
	}
	for _, id := range []string{"srv_unknown", "srv_platform"} {
		for _, key := range []string{"member", "sa_second", "admin", "op"} {
			want := 403
			if key == "admin" || key == "op" {
				want = 200
			}
			if code, out := f.do(t, f.keys[key], http.MethodPatch, "/v1/servers/"+id, patch); code != want {
				t.Errorf("%s %s: %d %v", key, id, code, out)
			}
		}
	}
	// Upcoming SA-to-person conversion reassigns retained keys. Resolve the
	// key's current identity, not a deleted account or the key's display name.
	execSQL(t, f.s, ctx, `UPDATE api_keys SET service_account_id=NULL,user_id='usr_member2' WHERE service_account_id='sa_bot'`)
	execSQL(t, f.s, ctx, `DELETE FROM service_accounts WHERE id='sa_bot'`)
	for _, key := range []string{"member2", "sa_second", "member"} {
		want := 200
		if key == "member" {
			want = 403
		}
		if code, out := f.do(t, f.keys[key], http.MethodPatch, "/v1/servers/srv_key", patch); code != want {
			t.Errorf("reassigned retained key, %s: %d %v", key, code, out)
		}
	}
	if got := queryOne[string](t, f.s, `SELECT owner FROM run_servers WHERE id='srv_key'`); got != "k_sa" {
		t.Fatalf("creator rewritten: %s", got)
	}
}

func TestStandaloneServerAccessIdentity(t *testing.T) {
	f, access := accessFixture(t)
	f.addKey(t, "second", "user_id", "usr_member")
	token := access.sign("mia@example.com")
	code, out := accessDo(t, f.s, token, http.MethodPost, "/v1/servers", map[string]any{"name": "web", "port": 3000, "wake": "request", "lifetime": "owner"})
	if code != 201 {
		t.Fatal(code, out)
	}
	path := "/v1/servers/" + out["id"].(string)
	patch := map[string]any{"labels": map[string]string{"access": "yes"}}
	for _, key := range []string{"member", "second"} {
		if code, out := f.do(t, f.keys[key], http.MethodPatch, path, patch); code != 200 {
			t.Fatal(code, out)
		}
	}
	if code, out := accessDo(t, f.s, token, http.MethodPatch, path, patch); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := accessDo(t, f.s, access.sign("max@example.com"), http.MethodPatch, path, patch); code != 403 {
		t.Fatal(code, out)
	}
}

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

// Attaching judges the source server as locked: a server an admin detaches
// (making it the tenant's, admins only) while a member's attach waits on
// its row is not attached by the member to their other Run. Deterministic:
// the admin's real detachTx holds the row until the request blocks on it.
func TestAttachRechecksDetachedServer(t *testing.T) {
	for _, creator := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_unresolved", true: "original_creator"}[creator], func(t *testing.T) {
			testAttachRechecksDetachedServer(t, creator)
		})
	}
}

func testAttachRechecksDetachedServer(t *testing.T, creator bool) {
	f := peopleFixture(t)
	ownedRuns(t, f)
	ctx := context.Background()
	execSQL(t, f.s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, owner_user_id) VALUES ('r_mia_b', 't1', '{}', 'stopped', 'usr_member')`)
	execSQL(t, f.s, ctx, `INSERT INTO run_servers (id, host, tenant_id, run_id, name, port, state, labels, lifetime)
		VALUES ('srv_aaaaaaaaaaaaaaaa', 'web-aaaaaaaa', 't1', 'r_mia', 'web', 3000, 'stopped', '{}', 'owner')`)
	if creator {
		execSQL(t, f.s, ctx, `UPDATE run_servers SET owner = 'mia@example.com' WHERE id = 'srv_aaaaaaaaaaaaaaaa'`)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	detached := make(chan error, 1)
	go func() {
		detached <- f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			v, err := lockServer(ctx, tx, "t1", "srv_aaaaaaaaaaaaaaaa")
			if err != nil {
				return err
			}
			if err := detachTx(ctx, tx, "t1", v, "test", "admin"); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-detached:
		t.Fatal(err)
	}
	type res struct {
		code int
		out  map[string]any
	}
	done := make(chan res, 1)
	go func() {
		code, out := f.do(t, f.keys["member"], http.MethodPost, "/v1/servers/srv_aaaaaaaaaaaaaaaa/attach", map[string]any{"runId": "r_mia_b"})
		done <- res{code, out}
	}()
	if err := waitLocked(f.s, 1, nil); err != nil { // the request, on the server's row
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-detached; err != nil {
		t.Fatal(err)
	}
	r := <-done
	wantCode, wantRun := http.StatusForbidden, "NULL"
	if creator {
		wantCode, wantRun = http.StatusOK, "r_mia_b"
	}
	if r.code != wantCode {
		t.Errorf("attach of a server detached meanwhile: %d %v, want %d", r.code, r.out, wantCode)
	}
	if got := queryOne[string](t, f.s, `SELECT coalesce(run_id, 'NULL') FROM run_servers WHERE id = 'srv_aaaaaaaaaaaaaaaa'`); got != wantRun {
		t.Errorf("server attached to %s, want %s", got, wantRun)
	}
	// An admin may attach either an unresolved or creator-owned server.
	if code, out := f.do(t, f.keys["admin"], http.MethodPost, "/v1/servers/srv_aaaaaaaaaaaaaaaa/attach", map[string]any{"runId": "r_mia_b"}); code != http.StatusOK || out["runId"] != "r_mia_b" {
		t.Errorf("admin attach: %d %v", code, out)
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
