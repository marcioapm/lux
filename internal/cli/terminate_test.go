package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lux terminate posts to /terminate; lux cancel, its hidden deprecated
// alias, posts there too and says on stderr that it is deprecated. Only
// terminate is listed in help.
func TestTerminateAndCancelAlias(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		deprecated bool
	}{{"terminate", false}, {"cancel", true}} {
		t.Run(c.cmd, func(t *testing.T) {
			var posted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posted = append(posted, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"run_1","state":"stopping"}`))
			}))
			defer srv.Close()
			var out, errOut strings.Builder
			a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
			root := a.root()
			root.SetArgs([]string{"--url", srv.URL, "--api-key", "k", c.cmd, "run_1"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if len(posted) != 1 || posted[0] != "POST /v1/runs/run_1/terminate" {
				t.Fatalf("requests: %v", posted)
			}
			if got := strings.Contains(errOut.String(), "deprecated"); got != c.deprecated {
				t.Fatalf("stderr %q", errOut.String())
			}
			if c.deprecated && strings.Count(strings.TrimSpace(errOut.String()), "\n") != 0 {
				t.Fatalf("deprecation note is more than one line: %q", errOut.String())
			}
			if strings.TrimSpace(out.String()) != "run_1 stopping" {
				t.Fatalf("stdout %q", out.String())
			}
		})
	}
	var help strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &help, stderr: &help}
	root := a.root()
	root.SetOut(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "terminate") || strings.Contains(help.String(), "  cancel") {
		t.Fatalf("help:\n%s", help.String())
	}
}
