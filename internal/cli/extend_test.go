package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lux extend posts exactly one of add, set or unlimited to
// /v1/runs/<run>/timeout and prints the Run's new timeout and time left.
func TestExtend(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if body["set"] == "1m" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"timeout_too_short","message":"timeout 1m0s is too short: the Run has run 50m0s, so the least it may be given is 51m0s"}}`))
			return
		}
		run := map[string]any{"id": "run_1", "state": "running", "timeout": "3h0m0s", "timeoutRemainingSeconds": 4500.5}
		if body["unlimited"] == true {
			run = map[string]any{"id": "run_1", "state": "running"}
		}
		out, _ := json.Marshal(run)
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	if code, out, errOut := lux(srv.URL, "extend", "run_1", "1h"); code != 0 || out != "run_1 timeout 3h0m0s (1h15m left)\n" {
		t.Fatalf("add: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := lux(srv.URL, "extend", "run_1", "--to", "3h"); code != 0 || out != "run_1 timeout 3h0m0s (1h15m left)\n" {
		t.Fatalf("set: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := lux(srv.URL, "extend", "run_1", "--unlimited"); code != 0 || out != "run_1 timeout none\n" {
		t.Fatalf("unlimited: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := lux(srv.URL, "extend", "run_1", "--to", "1m"); code != 4 || out != "" || !strings.Contains(errOut, "has run 50m0s") {
		t.Fatalf("too short: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := []string{
		`POST /v1/runs/run_1/timeout {"add":"1h"}`,
		`POST /v1/runs/run_1/timeout {"set":"3h"}`,
		`POST /v1/runs/run_1/timeout {"unlimited":true}`,
		`POST /v1/runs/run_1/timeout {"set":"1m"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, args := range [][]string{{"extend", "run_1"}, {"extend", "run_1", "1h", "--to", "2h"}, {"extend", "run_1", "--to", "2h", "--unlimited"}} {
		if code, _, errOut := lux(srv.URL, args...); code == 0 || !strings.Contains(errOut, "exactly one of") {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("refused flags sent requests: %q", got[len(want):])
	}
}

// lux resume --timeout sends timeout at the top of the resume body.
func TestResumeTimeoutFlag(t *testing.T) {
	sent := func(args ...string) map[string]any {
		t.Helper()
		var body map[string]any
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
				_, _ = w.Write([]byte(`{"id":"run_1","state":"failed"}`))
			case r.Method == "POST" && r.URL.Path == "/v1/runs/run_1/resume":
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming"}`))
			default:
				http.NotFound(w, r)
			}
		})
		if _, err := runCLI(t, h, append([]string{"resume", "run_1"}, args...)...); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := sent("--timeout", "4h"); body["timeout"] != "4h" || body["resources"] != nil {
		t.Errorf("--timeout 4h: body %v", body)
	}
	if body := sent("--timeout", "0s"); body["timeout"] != "0s" {
		t.Errorf("--timeout 0s: body %v", body)
	}
	if body := sent(); body["timeout"] != nil {
		t.Errorf("no flag: body %v", body)
	}
}

// lux get shows the timeout and what it leaves.
func TestGetShowsTimeout(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run_1","state":"running","timeout":"2h0m0s","timeoutRemainingSeconds":61}`))
	})
	out, err := runCLI(t, h, "get", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timeout:   2h0m0s (1m1s left)\n") {
		t.Fatalf("get:\n%s", out)
	}
}
