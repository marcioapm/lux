package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/ids"
)

// flowFixture: tenants ta and tb with read keys (a, b) and an operator key
// (op); platform pools pa, pb and pc with hosts ha (pa), hb (pb), and hn
// with no pool. The sampler's next window is (now - 20m, now - 1m].
func flowFixture(t *testing.T, s *Server) (map[string]string, time.Time) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('tb', 'b')`)
	keys := map[string]string{"a": ids.Secret("luxk"), "b": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('ka', 'ta', 'k', $1, ARRAY['read']), ('kb', 'tb', 'k', $2, ARRAY['read']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["a"]), ids.Hash(keys["b"]), ids.Hash(keys["op"]))
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared) VALUES
		('pa', NULL, 'a', 'static', true), ('pb', NULL, 'b', 'static', true), ('pc', NULL, 'c', 'static', true)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, capacity) VALUES
		('ha', NULL, 'ha', 'pa', 'ready', '{"cpus": 4}'), ('hb', NULL, 'hb', 'pb', 'ready', '{"cpus": 4}'),
		('hn', NULL, 'hn', NULL, 'ready', '{"cpus": 4}')`)
	execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, window_end) VALUES ('', 0, now() - interval '1 hour', now() - interval '20 minutes')`)
	var now time.Time
	systemScan(t, s, `SELECT now()`, nil, &now)
	return keys, now
}

// flowPlacement is a placement on host that reached running at started
// (zero: never).
type flowPlacement struct {
	host    string
	started time.Time
}

// flowRun adds a Run of tenant bound to pool (runs.pool_id) with its
// placements as epochs 1, 2, ...; started and finished zero are NULL. A
// finished Run is succeeded, an unfinished one running on its last epoch.
func flowRun(t *testing.T, s *Server, id, tenant, pool string, started, finished time.Time, pls ...flowPlacement) {
	t.Helper()
	ctx := context.Background()
	at := func(v time.Time) any {
		if v.IsZero() {
			return nil
		}
		return v
	}
	state := "running"
	if !finished.IsZero() {
		state = "succeeded"
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, first_started_at, finished_at, name)
		VALUES ($1, $2, '{}', $3, $4, $5, $6, $7, $1)`, id, tenant, state, pool, len(pls), at(started), at(finished))
	for i, pl := range pls {
		plState := "exited"
		if state == "running" && i == len(pls)-1 {
			plState = "running"
		}
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at, resources)
			VALUES ($1 || '-' || $4::int, $2, $1, $3, $4::int, $5, $6, '{"cpus": 1}')`, id, tenant, pl.host, i+1, plState, at(pl.started))
	}
}

// flowSamples is every raw pool sample's started/finished, by pool and
// tenant ("" for the pool's own row): "pa/": "5/1".
func flowSamples(t *testing.T, s *Server) map[string]string {
	t.Helper()
	got := map[string]string{}
	rows := queryOne[[]string](t, s, `SELECT coalesce(array_agg(pool_id || '/' || tenant_id || '=' || started || '/' || finished ORDER BY 1), '{}')
		FROM pool_samples WHERE res = 0 AND (started > 0 OR finished > 0)`)
	for _, r := range rows {
		k, v, _ := strings.Cut(r, "=")
		got[k] = v
	}
	return got
}

// flowStats is runsStarted per pool from GET /v1/pools/stats?since=2h as who.
func flowStats(t *testing.T, s *Server, key string) map[string]int {
	t.Helper()
	var list struct{ Pools []PoolStats }
	if code := getJSON(t, s, key, "/v1/pools/stats?since=2h", &list); code != http.StatusOK {
		t.Fatalf("GET /v1/pools/stats: %d", code)
	}
	got := map[string]int{}
	for _, p := range list.Pools {
		if p.RunsStarted != 0 {
			got[p.ID] = p.RunsStarted
		}
	}
	return got
}

func sampleNow(t *testing.T, s *Server) {
	t.Helper()
	if err := s.sampleSystem(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func wantMap[V comparable](t *testing.T, what string, got, want map[string]V) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

// A Run first started on pa's host, moved by a resume to pb (runs.pool_id
// now pb) and finished on pb's host counts its start under pa and its finish
// under pb, once each, in the pool rows and its tenant's rows of the samples
// and in GET /v1/pools/stats; it is running and queued nowhere.
func TestPoolFlowMovedRun(t *testing.T) {
	s := testServer(t)
	keys, now := flowFixture(t, s)
	m := func(d time.Duration) time.Time { return now.Add(-d * time.Minute) }
	flowRun(t, s, "moved", "ta", "pb", m(15), m(5), flowPlacement{"ha", m(15)}, flowPlacement{"hb", m(10)})
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"pa/": "1/0", "pa/ta": "1/0", "pb/": "0/1", "pb/ta": "0/1"})
	wantMap(t, "operator stats", flowStats(t, s, keys["op"]), map[string]int{"pa": 1})
	wantMap(t, "a's stats", flowStats(t, s, keys["a"]), map[string]int{"pa": 1})
	wantMap(t, "b's stats", flowStats(t, s, keys["b"]), map[string]int{})
}

// The start is the placement that first ran: the one whose started_at is
// first_started_at even when another started earlier, else the earliest;
// running stays on runs.pool_id, where the Run is now.
func TestPoolFlowFirstRunningPlacement(t *testing.T) {
	s := testServer(t)
	keys, now := flowFixture(t, s)
	m := func(d time.Duration) time.Time { return now.Add(-d * time.Minute) }
	// started_at of epoch 2 is first_started_at: pa, not hb's earlier epoch 1.
	flowRun(t, s, "match", "ta", "pb", m(9), time.Time{}, flowPlacement{"hb", m(40)}, flowPlacement{"ha", m(9)}, flowPlacement{"hb", m(3)})
	// No placement's started_at is first_started_at: the earliest, pa's.
	flowRun(t, s, "earliest", "tb", "pb", m(8), time.Time{}, flowPlacement{"ha", m(7)}, flowPlacement{"hb", m(6)})
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"pa/": "2/0", "pa/ta": "1/0", "pa/tb": "1/0"})
	if got := queryOne[string](t, s, `SELECT string_agg(pool_id || '/' || tenant_id || '=' || running, ' ' ORDER BY 1)
		FROM pool_samples WHERE res = 0 AND running > 0`); got != "pb/=2 pb/ta=1 pb/tb=1" {
		t.Errorf("running %q, want both on pb", got)
	}
	wantMap(t, "operator stats", flowStats(t, s, keys["op"]), map[string]int{"pa": 2})
	wantMap(t, "b's stats", flowStats(t, s, keys["b"]), map[string]int{"pa": 1})
}

// A Run that never moved counts in its pool for both.
func TestPoolFlowSamePool(t *testing.T) {
	s := testServer(t)
	keys, now := flowFixture(t, s)
	m := func(d time.Duration) time.Time { return now.Add(-d * time.Minute) }
	flowRun(t, s, "stay", "tb", "pa", m(14), m(4), flowPlacement{"ha", m(14)}, flowPlacement{"ha", m(9)})
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"pa/": "1/1", "pa/tb": "1/1"})
	wantMap(t, "b's stats", flowStats(t, s, keys["b"]), map[string]int{"pa": 1})
	wantMap(t, "a's stats", flowStats(t, s, keys["a"]), map[string]int{})
}

// No placement, or a placement whose host has no pool: runs.pool_id.
func TestPoolFlowFallbacks(t *testing.T) {
	s := testServer(t)
	keys, now := flowFixture(t, s)
	m := func(d time.Duration) time.Time { return now.Add(-d * time.Minute) }
	flowRun(t, s, "none", "ta", "pc", m(13), m(3))
	flowRun(t, s, "nopool", "tb", "pc", m(12), m(2), flowPlacement{"hn", m(12)})
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"pc/": "2/2", "pc/ta": "1/1", "pc/tb": "1/1"})
	wantMap(t, "operator stats", flowStats(t, s, keys["op"]), map[string]int{"pc": 2})
	wantMap(t, "a's stats", flowStats(t, s, keys["a"]), map[string]int{"pc": 1})
}

// A moved Run whose start is in the sample's window and finish after it
// counts its start only; one started before the window and finished in it,
// its finish only; one started before the stats range counts there nowhere.
func TestPoolFlowWindowEdges(t *testing.T) {
	s := testServer(t)
	keys, now := flowFixture(t, s)
	m := func(d time.Duration) time.Time { return now.Add(-d * time.Minute) }
	flowRun(t, s, "startin", "ta", "pb", m(11), now.Add(time.Minute), flowPlacement{"ha", m(11)}, flowPlacement{"hb", m(5)})
	flowRun(t, s, "finishin", "ta", "pb", m(30), m(6), flowPlacement{"ha", m(30)}, flowPlacement{"hb", m(25)})
	flowRun(t, s, "old", "tb", "pb", m(180), m(90), flowPlacement{"ha", m(180)}, flowPlacement{"hb", m(150)})
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"pa/": "1/0", "pa/ta": "1/0", "pb/": "0/1", "pb/ta": "0/1"})
	wantMap(t, "operator stats", flowStats(t, s, keys["op"]), map[string]int{"pa": 2})
	wantMap(t, "b's stats", flowStats(t, s, keys["b"]), map[string]int{})
}
