package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// matchingBins is a runner_bin_dir-equivalent map for one arch, both
// binaries present, hashing to "r1"/"s1": what a Hello or Heartbeat
// reporting those shas is considered up to date against.
func matchingBins() map[string]map[string]runnerBin {
	return map[string]map[string]runnerBin{"arm64": {"lux-runner": {sha256: "r1"}, "lux-shim": {sha256: "s1"}}}
}

// hostState reads back a host's draining flag, state, state_reason,
// drain_causes and its two timestamps as real values (not just
// presence): drain_requested_at is compared by value in
// TestHeartbeatDrainsForOutdatedBinaries, so a heartbeat that re-drained
// the host (even leaving the boolean columns unchanged) is caught.
func hostState(t *testing.T, s *Server, ctx context.Context, id string) (draining bool, state, reason string, causes []string, exitRequested *time.Time, drainRequested *time.Time) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT draining, state, state_reason, drain_causes, exit_requested_at, drain_requested_at
			FROM hosts WHERE id = $1`, id).Scan(&draining, &state, &reason, &causes, &exitRequested, &drainRequested)
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func exitMessageCount(t *testing.T, s *Server, ctx context.Context, id string) int {
	t.Helper()
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_messages WHERE host_id = $1 AND type = $2`, id, proto.MsgExit).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// hello registers host "h1" reporting runnerSHA/shimSHA for arm64 and
// returns its id.
func hello(t *testing.T, s *Server, ctx context.Context, tok *hostToken, runnerSHA, shimSHA string) string {
	t.Helper()
	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: runnerSHA, ShimSHA256: shimSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.HostID
}

// queueExit does what the reaper does once an outdated host is idle.
func queueExit(t *testing.T, s *Server, ctx context.Context, hostID string) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET exit_requested_at = now() WHERE id = $1`, hostID); err != nil {
			return err
		}
		return enqueue(ctx, tx, hostID, "", 0, proto.MsgExit, proto.ExitHost{Reason: outdatedBinariesReason, Code: proto.ExitCodeOutdatedBinaries})
	}); err != nil {
		t.Fatal(err)
	}
}

// testHostToken inserts a host_tokens row (registerHost's INSERT of a new
// host references it) and returns a *hostToken for it.
func testHostToken(t *testing.T, s *Server, ctx context.Context) *hostToken {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, token_hash) VALUES ('tok1', 'hash1')`)
	return &hostToken{ID: "tok1"}
}

// A Hello that still does not match the outdated binaries keeps the
// "outdated" cause and the reaper's exit_requested_at path working: a
// reconnect (luxd itself restarting, or a WebSocket blip) must not wipe
// the cause, and the reaper's queued exit must survive it.
func TestHelloReconnectWhileOutdatedPreservesReasonAndExitPath(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	// First Hello: mismatched shas, drains the host for outdated binaries.
	hostID := hello(t, s, ctx, tok, "old-r", "old-s")
	draining, state, reason, causes, _, drainReq := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !slices.Contains(causes, causeOutdated) || drainReq == nil {
		t.Fatalf("after first Hello: draining=%v state=%q reason=%q causes=%v drainRequested=%v", draining, state, reason, causes, drainReq)
	}

	// The reaper would now queue an exit once idle; simulate it directly.
	queueExit(t, s, ctx, hostID)

	// A reconnect before the runner has new binaries (luxd restarted, or
	// the WebSocket blipped): the Hello still reports the old shas.
	hello(t, s, ctx, tok, "old-r", "old-s")
	draining, state, reason, causes, exitReq, drainReq := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !slices.Contains(causes, causeOutdated) {
		t.Fatalf("after reconnect (still outdated): draining=%v state=%q reason=%q causes=%v, want still draining for outdated binaries", draining, state, reason, causes)
	}
	if exitReq == nil || drainReq == nil {
		t.Fatalf("after reconnect (still outdated): exitRequested=%v drainRequested=%v, want both still set (reaper's queued exit intact)", exitReq, drainReq)
	}
	if n := exitMessageCount(t, s, ctx, hostID); n != 1 {
		t.Fatalf("exit messages after reconnect: %d, want still 1", n)
	}
}

// Once a Hello reports matching binaries, the host un-drains: the reason,
// both timestamps, and any queued (redelivered) exit message are cleared
// in the same registerHost call, so the reaper never re-exits a host that
// is current again.
func TestHelloUndrainsOnceBinariesMatch(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	hostID := hello(t, s, ctx, tok, "old-r", "old-s")
	queueExit(t, s, ctx, hostID)

	// The restart's ExecStartPre re-downloaded the binaries: this Hello
	// matches.
	hello(t, s, ctx, tok, "r1", "s1")
	draining, state, reason, causes, exitReq, drainReq := hostState(t, s, ctx, hostID)
	if draining || state != "ready" || reason != "" || len(causes) != 0 {
		t.Fatalf("after undrain: draining=%v state=%q reason=%q causes=%v, want ready with no reason or cause", draining, state, reason, causes)
	}
	if exitReq != nil || drainReq != nil {
		t.Fatalf("after undrain: exitRequested=%v drainRequested=%v, want both cleared", exitReq, drainReq)
	}
	var acked int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_messages WHERE host_id = $1 AND type = $2 AND acked_at IS NOT NULL`,
			hostID, proto.MsgExit).Scan(&acked)
	})
	if err != nil {
		t.Fatal(err)
	}
	if acked != 1 {
		t.Fatalf("acked exit messages after undrain: %d, want 1 (the stale queued exit acked, not redelivered)", acked)
	}
}

// A host already draining for a reason luxd did not set (a person asked
// for it) must never be touched by the outdated-binaries logic in either
// direction: the cause survives a Hello, and the host is not undrained
// just because its binaries happen to match.
func TestHelloLeavesAnOtherReasonDrainAlone(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	hostID := hello(t, s, ctx, tok, "r1", "s1") // matches: not outdated
	execSQL(t, s, ctx, `UPDATE hosts SET draining = true, state = 'draining', state_reason = 'drain requested',
		drain_causes = ARRAY[$2] WHERE id = $1`, hostID, causeManual)

	hello(t, s, ctx, tok, "r1", "s1")
	draining, state, reason, causes, _, _ := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || reason != "drain requested" || !slices.Contains(causes, causeManual) {
		t.Fatalf("a manual drain was disturbed: draining=%v state=%q reason=%q causes=%v", draining, state, reason, causes)
	}
}

// The heartbeat's drain branch: a host already draining (for any reason)
// is left alone, and one that is not, and reports outdated binaries, is
// cordoned (no requestStop; only a Hello or the reaper act further). A
// second heartbeat, still outdated, must not re-drain: compared by real
// values (the drain_requested_at timestamp, the host_messages count and
// the cause set), not by presence, which a re-drain would not disturb
// either.
func TestHeartbeatDrainsForOutdatedBinaries(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	hostID := hello(t, s, ctx, tok, "r1", "s1")
	if err := s.heartbeat(ctx, hostID, proto.Heartbeat{RunnerSHA256: "old-r", ShimSHA256: "old-s"}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, causes, _, drainReq1 := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !slices.Contains(causes, causeOutdated) || drainReq1 == nil {
		t.Fatalf("after heartbeat reporting outdated binaries: draining=%v state=%q reason=%q causes=%v drainRequested=%v", draining, state, reason, causes, drainReq1)
	}
	msgs1 := exitMessageCount(t, s, ctx, hostID)

	// A second heartbeat, still outdated, must not re-drain: the
	// drain_requested_at timestamp, host_messages count and cause set
	// must all be unchanged (a boolean presence check cannot fail here,
	// since a re-drain would not clear draining or the timestamp column).
	if err := s.heartbeat(ctx, hostID, proto.Heartbeat{RunnerSHA256: "old-r", ShimSHA256: "old-s"}); err != nil {
		t.Fatal(err)
	}
	_, _, _, causes2, _, drainReq2 := hostState(t, s, ctx, hostID)
	if drainReq1 == nil || drainReq2 == nil || !drainReq1.Equal(*drainReq2) {
		t.Fatalf("a second outdated heartbeat moved drain_requested_at: %v -> %v", drainReq1, drainReq2)
	}
	if msgs2 := exitMessageCount(t, s, ctx, hostID); msgs2 != msgs1 {
		t.Fatalf("a second outdated heartbeat changed host_messages: %d -> %d", msgs1, msgs2)
	}
	if !slices.Equal(causes, causes2) {
		t.Fatalf("a second outdated heartbeat changed drain_causes: %v -> %v", causes, causes2)
	}
}

// Two full release cycles on one static host: drain, exit (simulated),
// matching Hello (undrain), a second mismatch, drain, exit again. The
// reaper must be able to enqueue a second exit after the first release's
// timestamps were cleared on undrain.
func TestTwoReleaseCyclesEachExitOnce(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	// Release 1: luxd holds "r1"/"s1"; the runner starts on an older pair.
	s.bins = matchingBins()
	hostID := hello(t, s, ctx, tok, "r0", "s0")
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, hostID); n != 1 {
		t.Fatalf("release 1: exit messages = %d, want 1", n)
	}
	// The unit restarts; ExecStartPre re-downloads r1/s1: a matching Hello
	// undrains the host, clearing exit_requested_at and drain_requested_at.
	hello(t, s, ctx, tok, "r1", "s1")
	draining, _, _, _, exitReq, drainReq := hostState(t, s, ctx, hostID)
	if draining || exitReq != nil || drainReq != nil {
		t.Fatalf("after release 1's undrain: draining=%v exitRequested=%v drainRequested=%v, want all clear", draining, exitReq, drainReq)
	}

	// Release 2: luxd now holds "r2"/"s2". The same host, still on r1/s1,
	// heartbeats and is drained again.
	s.bins = map[string]map[string]runnerBin{"arm64": {"lux-runner": {sha256: "r2"}, "lux-shim": {sha256: "s2"}}}
	if err := s.heartbeat(ctx, hostID, proto.Heartbeat{RunnerSHA256: "r1", ShimSHA256: "s1"}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, causes, _, _ := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !slices.Contains(causes, causeOutdated) {
		t.Fatalf("release 2: draining=%v state=%q reason=%q causes=%v, want drained for outdated binaries again", draining, state, reason, causes)
	}
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, hostID); n != 2 {
		t.Fatalf("release 2: total exit messages = %d, want 2 (exit_requested_at from release 1 must have been cleared on undrain)", n)
	}
}

// A static host that goes lost mid-update (its restart ran past the
// lease) is not stranded: reapHosts overwrites state_reason (display
// text only), but drain_causes and the "outdated" cause it carries
// survive, so a later matching Hello still undrains it.
func TestLostMidUpdateStillUndrainsOnMatchingHello(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	hostID := hello(t, s, ctx, tok, "old-r", "old-s")
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, hostID); n != 1 {
		t.Fatalf("exit messages before the lost window: %d, want 1", n)
	}

	// The restart takes longer than the lease: reapHosts marks it lost,
	// overwriting state_reason (display text) with "missed heartbeats".
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = $1`, hostID)
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
	_, state, reason, causes, _, _ := hostState(t, s, ctx, hostID)
	if state != "lost" || reason != "missed heartbeats" {
		t.Fatalf("after reapHosts: state=%q reason=%q, want lost/missed heartbeats", state, reason)
	}
	if !slices.Contains(causes, causeOutdated) {
		t.Fatalf("reapHosts dropped drain_causes: %v, want it to still hold %q", causes, causeOutdated)
	}

	// ExecStartPre re-downloaded the binaries: the matching Hello that
	// follows the restart must undrain the host, exactly as if it had
	// never gone lost.
	hello(t, s, ctx, tok, "r1", "s1")
	draining, state, _, causes, exitReq, drainReq := hostState(t, s, ctx, hostID)
	if draining || state != "ready" || len(causes) != 0 {
		t.Fatalf("after the matching Hello: draining=%v state=%q causes=%v, want ready with no causes", draining, state, causes)
	}
	if exitReq != nil || drainReq != nil {
		t.Fatalf("after the matching Hello: exitRequested=%v drainRequested=%v, want both cleared", exitReq, drainReq)
	}
}

// Outdated then manual, and manual then outdated: whichever order, a
// binaries-matching Hello removes only the "outdated" cause and the host
// stays draining for "manual" — an operator's drain is never silently
// undone by a release. The reaper must not send this host an exit either
// order: a manual drain means the operator owns the host now. Both
// causes are added through drainHosts directly: a heartbeat or Hello
// leaves an already-draining host alone, so could never add "outdated"
// after "manual".
func TestOutdatedAndManualDrainCoexistAcrossAnUndrain(t *testing.T) {
	for _, order := range []string{"outdated-then-manual", "manual-then-outdated"} {
		t.Run(order, func(t *testing.T) {
			s := testServer(t)
			s.bins = matchingBins()
			ctx := context.Background()
			tok := testHostToken(t, s, ctx)

			hostID := hello(t, s, ctx, tok, "r1", "s1") // matches: starts not outdated

			drain := func(reason, cause string) {
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					_, err := s.drainHosts(ctx, tx, reason, cause, "", "id = $1", hostID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			drainOutdated := func() { drain(outdatedBinariesReason, causeOutdated) }
			drainManual := func() { drain("drain requested", causeManual) }
			if order == "outdated-then-manual" {
				drainOutdated()
				drainManual()
			} else {
				drainManual()
				drainOutdated()
			}

			draining, state, _, causes, _, _ := hostState(t, s, ctx, hostID)
			if !draining || state != "draining" || !slices.Contains(causes, causeOutdated) || !slices.Contains(causes, causeManual) {
				t.Fatalf("%s: draining=%v state=%q causes=%v, want both outdated and manual present", order, draining, state, causes)
			}

			// The reaper must not exit a host also carrying "manual": the
			// operator owns it now.
			if err := s.reapOutdatedStaticHosts(ctx); err != nil {
				t.Fatal(err)
			}
			if n := exitMessageCount(t, s, ctx, hostID); n != 0 {
				t.Fatalf("%s: the reaper sent an exit to a host also manually drained: %d messages", order, n)
			}

			// Binaries now match again (a restart re-downloaded them): the
			// Hello must remove only "outdated" and leave the host draining
			// for "manual".
			hello(t, s, ctx, tok, "r1", "s1")
			draining, state, _, causes, _, _ = hostState(t, s, ctx, hostID)
			if !draining || state != "draining" || slices.Contains(causes, causeOutdated) || !slices.Contains(causes, causeManual) {
				t.Fatalf("%s: after the matching Hello: draining=%v state=%q causes=%v, want still draining for manual only", order, draining, state, causes)
			}
		})
	}
}

// A force-evict on a host already draining for outdated binaries is not
// undone by the matching Hello either: the operator's cause ("manual")
// and the placement's stop survive.
func TestForceEvictOnAnOutdatedHostSurvivesTheUpdate(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, token_hash) VALUES ('tok1', 't1', 'hash1')`)
	t1 := "t1"
	tok := &hostToken{ID: "tok1", TenantID: &t1}

	hostID := hello(t, s, ctx, tok, "old-r", "old-s")
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', $1, 1, 'running')`, hostID)

	octx := context.WithValue(ctx, principalKey, Principal{Operator: true, Scopes: []string{"admin", "operator"}})
	in := &drainHostInput{HostPath: HostPath{ID: hostID}, Body: &drainHostRequest{ForceEvict: true}}
	if _, err := s.drainHost(octx, in); err != nil {
		t.Fatal(err)
	}
	draining, stopRequested, stopReason := drainState(t, s, ctx, hostID, "r1")
	if !draining || !stopRequested || stopReason != "drain" {
		t.Fatalf("after force-evict: draining=%v stopRequested=%v stopReason=%q", draining, stopRequested, stopReason)
	}

	// A matching Hello (its restart's ExecStartPre re-downloaded the
	// binaries) must remove only "outdated" and leave the host draining
	// for the operator's force-evict.
	hello(t, s, ctx, tok, "r1", "s1")
	draining, state, _, causes, _, _ := hostState(t, s, ctx, hostID)
	if !draining || state != "draining" || slices.Contains(causes, causeOutdated) || !slices.Contains(causes, causeManual) {
		t.Fatalf("after the matching Hello: draining=%v state=%q causes=%v, want still draining for manual only", draining, state, causes)
	}
}
