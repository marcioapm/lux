package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// Run.resumable is the ?resumable=true filter, Run by Run, and both say
// whether a resume without fromSnapshot is accepted: for each state (and a
// never Run, and a Run left with only a refused snapshot report), the flag
// on GET /v1/runs/{id} and on the list, the filter's membership and the
// resume's outcome agree, and the flag is always present in the JSON.
func TestResumableFlagIsTheFilter(t *testing.T) {
	cases := []struct {
		name, state, policy string
		refusedNoSnapshot   bool
		want                bool
	}{
		{"stopped", StateStopped, "", false, true},
		{"lost", StateLost, "", false, true},
		{"failed", StateFailed, "", false, true},
		{"succeeded", StateSucceeded, "", false, true},
		{"terminated", StateTerminated, "", false, false},
		{"running", StateRunning, "", false, false},
		{"never", StateStopped, "never", false, false},
		{"refused-no-snapshot", StateStopped, "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := policyFixture(t, c.policy)
			execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'r1'`, c.state)
			if c.state != StateRunning {
				execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
			}
			if c.refusedNoSnapshot {
				execSQL(t, s, ctx, `UPDATE placements SET snapshot_refused = true WHERE id = 'p1'`)
			} else if c.state != StateRunning {
				exitedSnapshot(t, s, ctx)
			}
			key := apiKey(t, s, new("t1"), "run", "read")

			code, body := call(t, s, key, http.MethodGet, "/v1/runs/r1", nil)
			if code != http.StatusOK {
				t.Fatalf("GET: %d %s", code, body)
			}
			got := resumableOf(t, body)

			code, body = call(t, s, key, http.MethodGet, "/v1/runs?limit=10", nil)
			if code != http.StatusOK {
				t.Fatalf("list: %d %s", code, body)
			}
			var list struct{ Runs []json.RawMessage }
			if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Runs) != 1 {
				t.Fatalf("list: %s %v", body, err)
			}
			inList := resumableOf(t, string(list.Runs[0]))

			filtered, err := s.listRuns(tenantCtx("t1"), &listRunsInput{Resumable: true})
			if err != nil {
				t.Fatal(err)
			}
			inFilter := slices.ContainsFunc(filtered.Body.Runs, func(r *Run) bool { return r.ID == "r1" })

			if got != c.want || inList != c.want || inFilter != c.want {
				t.Fatalf("resumable: GET %v, list %v, filter %v; want %v", got, inList, inFilter, c.want)
			}
			// The flag tells what resume does.
			_, err = s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}})
			if accepted := err == nil; accepted != c.want {
				t.Fatalf("resume accepted %v (%v), flag %v", accepted, err, c.want)
			}
		})
	}
}

// The flag is on the Run every answer carries: submit, stop and terminate
// too, as of the answer.
func TestResumableFlagOnActionAnswers(t *testing.T) {
	s, _ := policyFixture(t, "")
	key := apiKey(t, s, new("t1"), "run", "read")
	code, body := call(t, s, key, http.MethodPost, "/v1/runs/r1/stop", nil)
	if code != http.StatusAccepted || resumableOf(t, body) {
		t.Fatalf("stop of a running Run: %d, resumable %v (still stopping)", code, resumableOf(t, body))
	}
	exitR1(t, s)
	code, body = call(t, s, key, http.MethodGet, "/v1/runs/r1", nil)
	if code != http.StatusOK || !resumableOf(t, body) {
		t.Fatalf("stopped: %d %s", code, body)
	}
	code, body = call(t, s, key, http.MethodPost, "/v1/runs/r1/terminate", nil)
	if code != http.StatusAccepted || resumableOf(t, body) {
		t.Fatalf("terminate: %d %s", code, body)
	}
}

// exitedSnapshot gives r1 an uploaded snapshot of its exited placement, so
// a resume has one to restore.
func exitedSnapshot(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snapR1', 't1', 'r1', 'p1', 1, '{"volumes": []}', 'h1', true)`)
	execSQL(t, s, ctx, `UPDATE runs SET snapshot_id = 'snapR1' WHERE id = 'r1'`)
}

// resumableOf reads a Run's resumable from its JSON, failing if absent.
func resumableOf(t *testing.T, body string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	v, ok := m["resumable"].(bool)
	if !ok {
		t.Fatalf("no resumable bool in %s", body)
	}
	return v
}
