package server

import (
	"context"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// How each end leaves a Run, by resumePolicy: a never Run, which nothing
// can resume, ends terminated whenever it would otherwise rest resumable
// (success, a requested stop, a failure, a move), its outcome in the
// reason and its exit code kept; every other policy rests resumable, as
// before. A terminate ends terminated, whatever the policy.
func TestNeverRunEndsTerminated(t *testing.T) {
	type end struct {
		name  string
		do    func(t *testing.T, s *Server)
		state map[string]string // policy ("" for auto) -> state
		// reason when it ends terminated by its policy never
		neverReason string
		exitCode    int
	}
	stopThen := func(code int) func(t *testing.T, s *Server) {
		return func(t *testing.T, s *Server) {
			if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			exitR1With(t, s, code)
		}
	}
	ends := []end{
		{"success", func(t *testing.T, s *Server) { exitR1With(t, s, 0) },
			map[string]string{"": StateSucceeded, "auto": StateSucceeded, "restart": StateSucceeded, "manual": StateSucceeded, "never": StateTerminated},
			"succeeded; resumePolicy never", 0},
		{"failure", func(t *testing.T, s *Server) { exitR1With(t, s, 1) },
			map[string]string{"": StateFailed, "auto": StateFailed, "restart": StateFailed, "manual": StateFailed, "never": StateTerminated},
			"exit code 1; resumePolicy never", 1},
		{"requested stop", stopThen(143),
			map[string]string{"": StateStopped, "auto": StateStopped, "restart": StateStopped, "manual": StateStopped, "never": StateTerminated},
			"stop; resumePolicy never", 143},
		{"preempt", func(t *testing.T, s *Server) {
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			exitR1With(t, s, 143)
		},
			// auto and restart: resuming again at once.
			map[string]string{"": StateResuming, "auto": StateResuming, "restart": StateResuming, "manual": StateFailed, "never": StateTerminated},
			"preempt: not resumed (resumePolicy never)", 143},
	}
	for _, e := range ends {
		for _, policy := range []string{"", "auto", "restart", "manual", "never"} {
			t.Run(e.name+"/"+policy, func(t *testing.T) {
				s, _ := policyFixture(t, policy)
				e.do(t, s)
				var state, reason string
				var exitCode *int
				var resumable, servers bool
				systemScan(t, s, `SELECT r.state, r.state_reason, r.exit_code, `+resumableSQL+`,
						EXISTS (SELECT 1 FROM run_servers WHERE run_id = r.id)
					FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &exitCode, &resumable, &servers)
				if want := e.state[policy]; state != want {
					t.Fatalf("state %q (%q), want %q", state, reason, want)
				}
				// A Run placed again starts its next placement without one.
				if state != StateResuming && (exitCode == nil || *exitCode != e.exitCode) {
					t.Fatalf("exit code %v, want %d", exitCode, e.exitCode)
				}
				if policy != "never" {
					return
				}
				_, held := s.secrets.get("r1")
				var ended string
				systemScan(t, s, `SELECT data->>'outcome' FROM host_events WHERE type = 'host.placement_ended' ORDER BY id DESC LIMIT 1`, nil, &ended)
				if reason != e.neverReason || resumable || held || servers || ended != StateTerminated {
					t.Fatalf("reason %q resumable %v secrets held %v servers %v placement outcome %q; want %q, nothing left",
						reason, resumable, held, servers, ended, e.neverReason)
				}
			})
		}
	}
}

// A never Run stopped while it waits for a host (no placement) ends
// terminated at once too, its reason saying so.
func TestNeverRunStoppedQueuedEndsTerminated(t *testing.T) {
	s, ctx := policyFixture(t, "never")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming' WHERE id = 'r1'`)
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "stop; resumePolicy never" {
		t.Fatalf("state %q reason %q", state, reason)
	}
}

// A failed Run with no snapshot (its only report refused) is not
// terminated for that: it rests failed and resumable unless its policy is
// never, which terminates it as any of its ends; the refusal is still
// named, after the policy.
func TestNoSnapshotIsNoTerminate(t *testing.T) {
	for policy, want := range map[string]string{"": StateFailed, "never": StateTerminated} {
		t.Run(policy, func(t *testing.T) {
			s, ctx := policyFixture(t, policy)
			execSQL(t, s, ctx, `UPDATE placements SET snapshot_refused = true WHERE id = 'p1'`)
			code := 1
			f := proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "r1", Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}
			if got := s.handleReport(context.Background(), "h1", f); got.Type != proto.MsgAck {
				t.Fatalf("exit report: %s %s", got.Type, got.Data)
			}
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
			wantReason := "exit code 1; " + refusedNoSnapshotReason
			if policy == "never" {
				wantReason = "exit code 1; resumePolicy never; " + refusedNoSnapshotReason
			}
			if state != want || reason != wantReason {
				t.Fatalf("state %q reason %q; want %q %q", state, reason, want, wantReason)
			}
		})
	}
}
