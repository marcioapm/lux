package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// matchingBins is a runner_bin_dir-equivalent map for one arch, both
// binaries present, hashing to "r1"/"s1": what a Hello or Heartbeat
// reporting those shas is considered up to date against.
func matchingBins() map[string]map[string]string {
	return map[string]map[string]string{"arm64": {"lux-runner": "r1", "lux-shim": "s1"}}
}

func hostState(t *testing.T, s *Server, ctx context.Context, id string) (draining bool, state, reason string, exitRequested, drainRequested bool) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT draining, state, state_reason, exit_requested_at IS NOT NULL, drain_requested_at IS NOT NULL
			FROM hosts WHERE id = $1`, id).Scan(&draining, &state, &reason, &exitRequested, &drainRequested)
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

// testHostToken inserts a host_tokens row (registerHost's INSERT of a new
// host references it) and returns a *hostToken for it.
func testHostToken(t *testing.T, s *Server, ctx context.Context) *hostToken {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, token_hash) VALUES ('tok1', 'hash1')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return &hostToken{ID: "tok1"}
}

// A Hello that still does not match the outdated binaries keeps the drain
// reason and the reaper's exit_requested_at path working: this is the
// exact reconnect race the drain-state bugs were found from (a Hello wipes
// state_reason unconditionally, or the reaper's timestamp is never reset).
func TestHelloReconnectWhileOutdatedPreservesReasonAndExitPath(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	// First Hello: mismatched shas, drains the host for outdated binaries.
	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "old-r", ShimSHA256: "old-s",
	})
	if err != nil {
		t.Fatal(err)
	}
	draining, state, reason, _, drainReq := hostState(t, s, ctx, w.HostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !drainReq {
		t.Fatalf("after first Hello: draining=%v state=%q reason=%q drainRequested=%v", draining, state, reason, drainReq)
	}

	// The reaper would now queue an exit once idle; simulate it directly.
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET exit_requested_at = now() WHERE id = $1`, w.HostID); err != nil {
			return err
		}
		return enqueue(ctx, tx, w.HostID, "", 0, proto.MsgExit, proto.ExitHost{Reason: outdatedBinariesReason, Code: proto.ExitCodeOutdatedBinaries})
	})
	if err != nil {
		t.Fatal(err)
	}

	// A reconnect before the runner has new binaries (luxd restarted, or
	// the WebSocket blipped): the Hello still reports the old shas.
	if _, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "old-r", ShimSHA256: "old-s",
	}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, exitReq, drainReq := hostState(t, s, ctx, w.HostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason {
		t.Fatalf("after reconnect (still outdated): draining=%v state=%q reason=%q, want still draining for outdated binaries", draining, state, reason)
	}
	if !exitReq || !drainReq {
		t.Fatalf("after reconnect (still outdated): exitRequested=%v drainRequested=%v, want both still set (reaper's queued exit intact)", exitReq, drainReq)
	}
	if n := exitMessageCount(t, s, ctx, w.HostID); n != 1 {
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

	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "old-r", ShimSHA256: "old-s",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET exit_requested_at = now() WHERE id = $1`, w.HostID); err != nil {
			return err
		}
		return enqueue(ctx, tx, w.HostID, "", 0, proto.MsgExit, proto.ExitHost{Reason: outdatedBinariesReason, Code: proto.ExitCodeOutdatedBinaries})
	})
	if err != nil {
		t.Fatal(err)
	}

	// The restart's ExecStartPre re-downloaded the binaries: this Hello
	// matches.
	if _, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r1", ShimSHA256: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, exitReq, drainReq := hostState(t, s, ctx, w.HostID)
	if draining || state != "ready" || reason != "" {
		t.Fatalf("after undrain: draining=%v state=%q reason=%q, want ready with no reason", draining, state, reason)
	}
	if exitReq || drainReq {
		t.Fatalf("after undrain: exitRequested=%v drainRequested=%v, want both cleared", exitReq, drainReq)
	}
	var acked int
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_messages WHERE host_id = $1 AND type = $2 AND acked_at IS NOT NULL`,
			w.HostID, proto.MsgExit).Scan(&acked)
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
// direction: the reason survives a Hello, and the host is not undrained
// just because its binaries happen to match.
func TestHelloLeavesAnOtherReasonDrainAlone(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r1", ShimSHA256: "s1", // matches: not outdated
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET draining = true, state = 'draining', state_reason = 'drain requested' WHERE id = $1`, w.HostID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r1", ShimSHA256: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, _, _ := hostState(t, s, ctx, w.HostID)
	if !draining || state != "draining" || reason != "drain requested" {
		t.Fatalf("a manual drain was disturbed: draining=%v state=%q reason=%q", draining, state, reason)
	}
}

// The heartbeat's drain branch: a host already draining (for any reason)
// is left alone, and one that is not, and reports outdated binaries, is
// cordoned (no requestStop; only a Hello or the reaper act further).
func TestHeartbeatDrainsForOutdatedBinaries(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	ctx := context.Background()
	tok := testHostToken(t, s, ctx)

	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r1", ShimSHA256: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.heartbeat(ctx, w.HostID, proto.Heartbeat{RunnerSHA256: "old-r", ShimSHA256: "old-s"}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, _, drainReq := hostState(t, s, ctx, w.HostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason || !drainReq {
		t.Fatalf("after heartbeat reporting outdated binaries: draining=%v state=%q reason=%q drainRequested=%v", draining, state, reason, drainReq)
	}

	// A second heartbeat, still outdated, must not re-drain (the reaper
	// test for host_messages count covers the reaper side; here the drain
	// timestamp and reason must be stable).
	drainedAt1 := drainReq
	if err := s.heartbeat(ctx, w.HostID, proto.Heartbeat{RunnerSHA256: "old-r", ShimSHA256: "old-s"}); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, drainReq2 := hostState(t, s, ctx, w.HostID)
	if drainedAt1 != drainReq2 {
		t.Fatal("a second outdated heartbeat re-drained the host")
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
	w, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r0", ShimSHA256: "s0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, w.HostID); n != 1 {
		t.Fatalf("release 1: exit messages = %d, want 1", n)
	}
	// The unit restarts; ExecStartPre re-downloads r1/s1: a matching Hello
	// undrains the host, clearing exit_requested_at and drain_requested_at.
	if _, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64",
		RunnerSHA256: "r1", ShimSHA256: "s1",
	}); err != nil {
		t.Fatal(err)
	}
	draining, _, _, exitReq, drainReq := hostState(t, s, ctx, w.HostID)
	if draining || exitReq || drainReq {
		t.Fatalf("after release 1's undrain: draining=%v exitRequested=%v drainRequested=%v, want all clear", draining, exitReq, drainReq)
	}

	// Release 2: luxd now holds "r2"/"s2". The same host, still on r1/s1,
	// heartbeats and is drained again.
	s.bins = map[string]map[string]string{"arm64": {"lux-runner": "r2", "lux-shim": "s2"}}
	if err := s.heartbeat(ctx, w.HostID, proto.Heartbeat{RunnerSHA256: "r1", ShimSHA256: "s1"}); err != nil {
		t.Fatal(err)
	}
	draining, state, reason, _, _ := hostState(t, s, ctx, w.HostID)
	if !draining || state != "draining" || reason != outdatedBinariesReason {
		t.Fatalf("release 2: draining=%v state=%q reason=%q, want drained for outdated binaries again", draining, state, reason)
	}
	if err := s.reapOutdatedStaticHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := exitMessageCount(t, s, ctx, w.HostID); n != 2 {
		t.Fatalf("release 2: total exit messages = %d, want 2 (this is the exact bug: exit_requested_at surviving release 1 would block this)", n)
	}
}
