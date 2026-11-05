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
