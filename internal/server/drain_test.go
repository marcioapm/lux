package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// drainState reads back a host's draining flag and its live placement's
// stop_requested_at and stop_reason (empty runID: only the host is checked).
func drainState(t *testing.T, s *Server, ctx context.Context, hostID, runID string) (draining bool, stopRequested bool, stopReason string) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT draining FROM hosts WHERE id = $1`, hostID).Scan(&draining); err != nil {
			return err
		}
		if runID == "" {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT stop_requested_at IS NOT NULL, stop_reason FROM placements WHERE run_id = $1`, runID).
			Scan(&stopRequested, &stopReason)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// drainFixture is a tenant with one ready host and one running Run placed
// on it (current_epoch = 1, matching its placement, so requestStop can
// find it): the common setup every drain test starts from.
func drainFixture(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)
}

// A plain drainHost cordons the host but leaves its live placement alone;
// forceEvict then stops it, even though the host is already draining.
func TestDrainHostCordonsThenForceEvictStopsRun(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})

	if _, err := s.drainHost(ctx, &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if draining, stopRequested, _ := drainState(t, s, context.Background(), "h1", "r1"); !draining || stopRequested {
		t.Fatalf("after plain drain: draining=%v stopRequested=%v", draining, stopRequested)
	}

	in := &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}}
	if _, err := s.drainHost(ctx, in); err != nil {
		t.Fatal(err)
	}
	draining, stopRequested, reason := drainState(t, s, context.Background(), "h1", "r1")
	if !draining || !stopRequested || reason != "drain" {
		t.Fatalf("after forceEvict on an already-draining host: draining=%v stopRequested=%v stopReason=%q, want draining/true/\"drain\"", draining, stopRequested, reason)
	}
}

// A POST with no body at all (curl scripts, and clients generated from
// the spec, send none) must still cordon the host cordon-only: the body
// is optional, not required.
func TestDrainHostHTTPWithNoBodyCordonsOnly(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('key1', 't1', 'k', $1, ARRAY['admin'])`, ids.Hash(key))

	h := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts/h1/drain", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /drain with no body: %d %s, want 202", w.Code, w.Body)
	}
	var body struct {
		Draining bool `json:"draining"`
	}
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Draining {
		t.Error("response says draining=false")
	}

	draining, stopRequested, _ := drainState(t, s, context.Background(), "h1", "r1")
	if !draining {
		t.Error("host was not cordoned")
	}
	if stopRequested {
		t.Error("a bodiless drain requested a stop; want cordon-only")
	}
}

// deletePool cordons the pool's provisioned hosts and retires the pool;
// only with forceEvict are their live Runs stopped.
func TestDeletePoolForceEvict(t *testing.T) {
	for _, forceEvict := range []bool{false, true} {
		t.Run(fmt.Sprintf("forceEvict=%v", forceEvict), func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
			execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
			execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provider_id) VALUES ('h1', 't1', 'h1', 'burst', 'ready', 'i-123')`)
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

			actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
			if _, err := s.deletePool(actx, &deletePoolInput{Name: "burst", ForceEvict: forceEvict}); err != nil {
				t.Fatal(err)
			}

			draining, stopRequested, reason := drainState(t, s, ctx, "h1", "r1")
			if !draining {
				t.Error("the pool's host was not cordoned")
			}
			if stopRequested != forceEvict || (forceEvict && reason != "drain") {
				t.Errorf("stopRequested=%v stopReason=%q, want a \"drain\" stop only with forceEvict", stopRequested, reason)
			}
			var retired bool
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT retired FROM pools WHERE id = 'pool1'`).Scan(&retired)
			}); err != nil {
				t.Fatal(err)
			}
			if !retired {
				t.Error("the pool was not retired")
			}
		})
	}
}

// deletePool never touches hosts outside the pool it removes: neither a
// platform host in another pool, nor a static host inside the very pool
// being deleted (deletePool only selects provider_id IS NOT NULL rows).
func TestDeletePoolOnlyTouchesItsOwnHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('h-other', 't1', 'h-other', 'default', 'ready')`)
	// A static host inside the pool being deleted: no provider_id, so it
	// stays uncordoned (the provisioner never terminates it either).
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('h-static', 't1', 'h-static', 'burst', 'ready')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &deletePoolInput{Name: "burst", ForceEvict: true}
	if _, err := s.deletePool(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, _, _ := drainState(t, s, context.Background(), "h-other", "")
	if draining {
		t.Error("deletePool cordoned a host outside the deleted pool")
	}
	draining, _, _ = drainState(t, s, context.Background(), "h-static", "")
	if draining {
		t.Error("deletePool cordoned a static host in the deleted pool")
	}
}

// A plain drain on a host already draining for outdated binaries adds its
// own cause ("manual") alongside "outdated" rather than clobbering it:
// both coexist in drain_causes, and state_reason is free to show
// whichever drain called last (display text, not a marker). Once
// "manual" is present the reaper leaves the host alone (the operator
// owns it now: see TestOutdatedAndManualDrainCoexistAcrossAnUndrain for
// the exit-skipped and undrain-order coverage); this test only checks
// that the earlier cause was not clobbered.
func TestPlainDrainKeepsAnEarlierDrainReason(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)

	// First, drained for outdated binaries (as drainIfOutdated does).
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := s.drainHosts(ctx, tx, outdatedBinariesReason, causeOutdated, "", "id = $1", "h1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, causes, _, _ := hostState(t, s, ctx, "h1"); !slices.Contains(causes, causeOutdated) {
		t.Fatalf("after the outdated-binaries drain: drain_causes = %v, want it to contain %q", causes, causeOutdated)
	}

	// Then a plain drain (an operator's) on the same host: must add its
	// own cause, not remove the earlier one.
	ctx = context.WithValue(ctx, principalKey, Principal{Operator: true, Scopes: []string{"admin", "operator"}})
	if _, err := s.drainHost(ctx, &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
		t.Fatal(err)
	}
	_, _, reason, causes, _, _ := hostState(t, s, ctx, "h1")
	if !slices.Contains(causes, causeOutdated) || !slices.Contains(causes, causeManual) {
		t.Fatalf("after a plain drain on top: drain_causes = %v, want both %q and %q", causes, causeOutdated, causeManual)
	}
	if reason != "drain requested" {
		t.Fatalf("after a plain drain on top: state_reason = %q, want the latest drain's display text", reason)
	}
}
