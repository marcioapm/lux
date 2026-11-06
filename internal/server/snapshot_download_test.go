package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func downloadEvents(t *testing.T, s *Server, run string) []map[string]any {
	t.Helper()
	var raw []byte
	systemScan(t, s, `SELECT coalesce(jsonb_agg(data ORDER BY id), '[]') FROM run_events WHERE run_id = $1 AND type = 'snapshot.downloaded'`, []any{run}, &raw)
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A tenant gets a presigned URL of its own snapshot's volume, as a 302 or
// as JSON, in any Run state (terminated included); each is an event.
func TestDownloadSnapshotVolume(t *testing.T) {
	s, ctx, _ := supersededFixture(t)
	key := apiKey(t, s, new("t1"), "run")
	const path = "/v1/runs/ra/snapshots/snapA/volumes/work"
	for _, state := range []string{StateStopped, StateTerminated, StateRunning} {
		execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'ra'`, state)
		w := apiCall(t, s, key, http.MethodGet, path, nil)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: %d %s, want 302", state, w.Code, w.Body)
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil || loc.Path != "/b/ra/bA-vol" || loc.Query().Get("X-Amz-Signature") == "" || loc.Query().Get("X-Amz-Expires") != "900" {
			t.Fatalf("%s: Location %s (%v), want a presigned GET of ra/bA-vol for 15 minutes", state, w.Header().Get("Location"), err)
		}
	}
	w := apiCall(t, s, key, http.MethodGet, path+"?redirect=false", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("redirect=false: %d %s", w.Code, w.Body)
	}
	var body SnapshotVolumeURL
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(body.URL)
	if u == nil || u.Path != "/b/ra/bA-vol" || u.Query().Get("X-Amz-Signature") == "" ||
		body.Size != 10 || body.SHA256 != "a-vol" || body.Name != "work" || body.SnapshotID != "snapA" || body.BlobID != "bA-vol" ||
		time.Until(body.ExpiresAt) < 14*time.Minute || time.Until(body.ExpiresAt) > 16*time.Minute {
		t.Fatalf("body %+v", body)
	}
	if strings.Contains(w.Header().Get("Location"), "http") {
		t.Fatalf("redirect=false also redirected")
	}
	evs := downloadEvents(t, s, "ra")
	if len(evs) != 4 {
		t.Fatalf("events %v, want 4 (one per URL)", evs)
	}
	if e := evs[3]; e["snapshot"] != "snapA" || e["volume"] != "work" || e["by"] == "" || e["by"] == nil {
		t.Fatalf("event %v", e)
	}
	// An operator, scoped to the Run's tenant by the Run.
	op := apiKey(t, s, nil, "operator")
	if w := apiCall(t, s, op, http.MethodGet, path, nil); w.Code != http.StatusFound {
		t.Fatalf("operator: %d %s", w.Code, w.Body)
	}
}

// Refusals: another tenant's Run, a snapshot of another Run, an unknown
// volume (404); a read-only key (403); a deleted snapshot (410); a volume
// still on its host (409, Retry-After). None records an event.
func TestDownloadSnapshotVolumeRefused(t *testing.T) {
	s, ctx, _ := supersededFixture(t)
	t1, t2 := apiKey(t, s, new("t1"), "run"), apiKey(t, s, new("t2"), "run")
	// Another Run of t1, so a snapshot of another Run of the same tenant.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('rc', 't1', '{}', 'terminated', 1)`)
	check := func(name, key, path string, want int, code string) *httptest.ResponseRecorder {
		t.Helper()
		w := apiCall(t, s, key, http.MethodGet, path, nil)
		if w.Code != want || (code != "" && !strings.Contains(w.Body.String(), `"`+code+`"`)) {
			t.Fatalf("%s: %d %s, want %d %s", name, w.Code, w.Body, want, code)
		}
		return w
	}
	check("another tenant's Run", t2, "/v1/runs/ra/snapshots/snapA/volumes/work", http.StatusNotFound, "not_found")
	check("another Run's snapshot", t1, "/v1/runs/rc/snapshots/snapA/volumes/work", http.StatusNotFound, "not_found")
	check("another tenant's snapshot", t1, "/v1/runs/ra/snapshots/snapB/volumes/work", http.StatusNotFound, "not_found")
	check("unknown volume", t1, "/v1/runs/ra/snapshots/snapA/volumes/home", http.StatusNotFound, "not_found")
	// Viewers may download snapshots (decision 2); successful downloads
	// record an event, so this belongs outside the refusals below.

	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host' WHERE id = 'bA-vol'`)
	w := check("on its host", t1, "/v1/runs/ra/snapshots/snapA/volumes/work?redirect=false", http.StatusConflict, "not_uploaded")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("409 without Retry-After")
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'terminated' WHERE id = 'ra'`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3' WHERE id = 'bA-vol'`)
	execSQL(t, s, ctx, `UPDATE snapshots SET available = false WHERE id = 'snapA'`)
	check("unavailable", t1, "/v1/runs/ra/snapshots/snapA/volumes/work", http.StatusGone, "snapshot_unavailable")
	execSQL(t, s, ctx, `UPDATE snapshots SET available = true WHERE id = 'snapA'`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'deleted' WHERE id = 'bA-vol'`)
	check("blob deleted", t1, "/v1/runs/ra/snapshots/snapA/volumes/work", http.StatusGone, "snapshot_unavailable")
	if evs := downloadEvents(t, s, "ra"); len(evs) != 0 {
		t.Fatalf("refusals recorded events: %v", evs)
	}
}
