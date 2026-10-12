package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// poolsAndResume is a fake luxd listing t1's pool "big" and the platform's
// "big" and "gpu", and taking resumes of run_1 (answer is its body, with
// status). posted receives each resume's body.
func poolsAndResume(t *testing.T, status int, answer string, posted *[]map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
			_, _ = w.Write([]byte(`{"id":"run_1","state":"running","secrets":[{"name":"TOKEN"}]}`))
		case r.Method == "GET" && r.URL.Path == "/v1/pools":
			_, _ = w.Write([]byte(`{"pools":[{"id":"pool_plat","name":"big","platform":true},{"id":"pool_mine","name":"big"},{"id":"pool_gpu","name":"gpu","platform":true}]}`))
		case r.Method == "POST" && r.URL.Path == "/v1/runs/run_1/resume":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("POST body %s: %v", b, err)
			}
			*posted = append(*posted, body)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(answer))
		default:
			http.NotFound(w, r)
		}
	})
}

// --pool sends placement.poolId: a name is the tenant's pool before the
// platform's, a platform-only name the platform's, an id itself; a name
// no pool has is refused before anything is POSTed.
func TestResumePoolFlag(t *testing.T) {
	for _, c := range []struct{ flag, want string }{{"big", "pool_mine"}, {"gpu", "pool_gpu"}, {"pool_plat", "pool_plat"}} {
		var posted []map[string]any
		h := poolsAndResume(t, http.StatusAccepted, `{"id":"run_1","state":"resuming","placement":{"pool":"big","poolId":"`+c.want+`"}}`, &posted)
		if _, err := runCLI(t, h, "resume", "run_1", "--secret", "TOKEN=x", "--pool", c.flag); err != nil {
			t.Fatal(err)
		}
		if len(posted) != 1 || !reflect.DeepEqual(posted[0]["placement"], map[string]any{"poolId": c.want}) {
			t.Errorf("--pool %s: POSTed %v, want placement.poolId %s", c.flag, posted, c.want)
		}
	}
	var posted []map[string]any
	_, err := runCLI(t, poolsAndResume(t, http.StatusAccepted, `{}`, &posted), "resume", "run_1", "--secret", "TOKEN=x", "--pool", "nope")
	if err == nil || !strings.Contains(err.Error(), `no pool "nope"`) || len(posted) != 0 {
		t.Fatalf("--pool nope: %v, posted %v", err, posted)
	}
}

// --dry-run sends validateOnly and prints "valid"; -o json prints the answer.
func TestResumeDryRun(t *testing.T) {
	var posted []map[string]any
	answer := `{"valid":true,"stateUnchecked":"running","placement":{"pool":"big","poolId":"pool_mine","platformUnchecked":true}}`
	out, err := runCLI(t, poolsAndResume(t, http.StatusOK, answer, &posted), "resume", "run_1", "--pool", "big", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0]["validateOnly"] != true {
		t.Fatalf("POSTed %v, want validateOnly", posted)
	}
	if out != "valid\n" {
		t.Errorf("output %q, want valid", out)
	}
	posted = nil
	out, err = runCLI(t, poolsAndResume(t, http.StatusOK, answer, &posted), "-o", "json", "resume", "run_1", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["valid"] != true || got["stateUnchecked"] != "running" {
		t.Errorf("json output %q: %v", out, err)
	}
}
