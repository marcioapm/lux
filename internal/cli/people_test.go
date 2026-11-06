package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeopleCommands(t *testing.T) {
	for _, tc := range []struct {
		args                       []string
		method, path, field, value string
		answer                     string
	}{
		{[]string{"whoami"}, "GET", "/v1/whoami", "", "", `{"role":"member","userId":"usr_1"}`},
		{[]string{"keys", "create", "--name", "desk", "--read-only", "--expires", "90d"}, "POST", "/v1/me/keys", "name", "desk", `{"apiKey":"lux_u_secret"}`},
		{[]string{"members", "set-role", "a@example.com", "admin"}, "PATCH", "/v1/members/a@example.com", "role", "admin", `{"role":"admin"}`},
		{[]string{"invites", "create", "a@example.com", "--role", "member"}, "POST", "/v1/invitations", "role", "member", `{"results":[]}`},
		{[]string{"sa", "create", "ci", "--role", "member", "--owner", "a@example.com"}, "POST", "/v1/service-accounts", "ownerEmail", "a@example.com", `{"name":"ci"}`},
		{[]string{"sa", "keys", "create", "ci", "--name", "rotation"}, "POST", "/v1/service-accounts/ci/keys", "name", "rotation", `{"apiKey":"lux_s_secret"}`},
		{[]string{"tenants", "create", "team", "--owner", "a@example.com"}, "POST", "/v1/tenants", "ownerEmail", "a@example.com", `{"id":"ten_1"}`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			hit := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("%s %s", r.Method, r.URL.Path)
				}
				if tc.field != "" {
					var b map[string]any
					json.NewDecoder(r.Body).Decode(&b)
					if b[tc.field] != tc.value {
						t.Error(b)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.answer))
			}))
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			a := &app{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
			args := append([]string{"--url", srv.URL, "--api-key", "key", "-o", "json"}, tc.args...)
			if code := a.main(args); code != 0 || !hit {
				t.Fatalf("%d %s", code, stderr.String())
			}
			if !json.Valid(stdout.Bytes()) {
				t.Fatal(stdout.String())
			}
		})
	}
}

// tenants create --owner: text shows the first owner's invitation (email,
// link, expiry), the thing the operator must send; JSON keeps it nested.
func TestTenantCreateOwnerInvitation(t *testing.T) {
	answer := `{"id":"ten_1","name":"team","tenantId":"ten_1","invitation":{"id":"inv_1","email":"a@example.com","role":"owner","status":"pending","code":"c0de","url":"https://lux.example.com/invite/c0de","createdAt":"2026-11-06T00:00:00Z","expiresAt":"2026-11-20T00:00:00Z"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	run := func(extra ...string) string {
		var stdout, stderr bytes.Buffer
		a := &app{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
		args := append([]string{"--url", srv.URL, "--api-key", "key"}, extra...)
		if code := a.main(append(args, "tenants", "create", "team", "--owner", "a@example.com")); code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		return stdout.String()
	}
	text := run()
	if !strings.Contains(text, "invitation: a@example.com  https://lux.example.com/invite/c0de  2026-11-20T00:00:00Z\n") {
		t.Errorf("text drops the invitation:\n%s", text)
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(run("-o", "json")), &got); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(answer), &want)
	if g, _ := json.Marshal(got); string(g) != func() string { w, _ := json.Marshal(want); return string(w) }() {
		t.Errorf("json changed: %s", g)
	}
}
