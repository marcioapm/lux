package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// snapshotR1Empty reports r1's placement epoch took snapshot snapID with no
// volume, as a transient (or never) runner exports on a non-move stop: its
// output and an artifact only.
func snapshotR1Empty(t *testing.T, s *Server, host string, epoch int, snapID string) {
	t.Helper()
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: snapID, RunID: "r1", Epoch: epoch, SessionID: "sess-1", Volumes: []proto.VolumeSnapshot{}},
		Output: &proto.BlobInfo{BlobID: "b-out-" + snapID, Size: 5, SHA256: "out-" + snapID},
		Artifacts: []proto.Artifact{{BlobInfo: proto.BlobInfo{BlobID: "b-art-" + snapID, Size: 6, SHA256: "art-" + snapID},
			Path: "/out/report.md", ContentType: "text/markdown", FileSize: 7, FileSHA256: "file-" + snapID}}}
	if got := reportSnapshot(t, s, host, "r1", epoch, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
		t.Fatalf("snapshot report: %s %s", got.Type, got.Data)
	}
}

// requestStopR1 stops r1's placement for reason, as the reaper or an API
// does.
func requestStopR1(t *testing.T, s *Server, reason string) {
	t.Helper()
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		_, err := s.requestStop(context.Background(), tx, "t1", "r1", reason)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A transient Run ends as a never Run does on every end but a move:
// terminated, its outcome and the policy in its reason, not resumable, its
// secrets and lifetime-run servers gone. Its exit snapshot (0 volumes, as
// its runner reports) is recorded with no volume.
func TestTransientRunEndsTerminated(t *testing.T) {
	for _, c := range []struct {
		name   string
		do     func(t *testing.T, s *Server)
		reason string
	}{
		{"success", func(t *testing.T, s *Server) {
			snapshotR1Empty(t, s, "h1", 1, "snapE")
			reportR1Exit(t, s, 0, "exited")
		}, "succeeded; resumePolicy transient"},
		{"failure", func(t *testing.T, s *Server) {
			snapshotR1Empty(t, s, "h1", 1, "snapE")
			reportR1Exit(t, s, 1, "exited")
		}, "exit code 1; resumePolicy transient"},
		{"requested stop", func(t *testing.T, s *Server) {
			if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			snapshotR1Empty(t, s, "h1", 1, "snapE")
			reportR1Exit(t, s, 143, "stopped")
		}, "stop; resumePolicy transient"},
		{"idle stop", func(t *testing.T, s *Server) {
			requestStopR1(t, s, "idle")
			snapshotR1Empty(t, s, "h1", 1, "snapE")
			reportR1Exit(t, s, 143, "stopped")
		}, "idle; resumePolicy transient"},
		{"host lost", loseR1Lease, "lease expired: host stopped heartbeating; resumePolicy transient"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := policyFixture(t, "transient")
			c.do(t, s)
			var state, reason string
			var resumable, servers bool
			systemScan(t, s, `SELECT r.state, r.state_reason, `+resumableSQL+`,
					EXISTS (SELECT 1 FROM run_servers WHERE run_id = r.id)
				FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &resumable, &servers)
			if state != StateTerminated || reason != c.reason {
				t.Fatalf("state %q reason %q, want terminated %q", state, reason, c.reason)
			}
			if resumable {
				t.Error("resumable")
			}
			if servers {
				t.Error("servers left")
			}
			if _, held := s.secrets.get("r1"); held {
				t.Error("secret values still held")
			}
			if c.name == "host lost" {
				return
			}
			var vols int
			systemScan(t, s, `SELECT jsonb_array_length(manifest->'volumes') FROM snapshots WHERE id = 'snapE'`, nil, &vols)
			if vols != 0 {
				t.Errorf("exit snapshot lists %d volumes, want 0", vols)
			}
		})
	}
}

// A transient Run stopped while it waits for a host ends terminated too.
func TestTransientRunStoppedQueuedEndsTerminated(t *testing.T) {
	s, ctx := policyFixture(t, "transient")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming' WHERE id = 'r1'`)
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "stop; resumePolicy transient" {
		t.Fatalf("state %q reason %q", state, reason)
	}
}

// A transient Run moved (preempt, drain, an operator's migrate, which is
// allowed) is placed again at once, as auto: resumed from the snapshot its
// move exported, with its session, its secrets kept and its server
// migrated.
func TestTransientRunResumesAfterAMove(t *testing.T) {
	for _, stop := range []string{"preempt", "drain", "migrate"} {
		t.Run(stop, func(t *testing.T) {
			s, ctx := policyFixture(t, "transient")
			if err := moveStops[stop](t, s); err != nil {
				t.Fatalf("%s: %v", stop, err)
			}
			if !r1Moving(t, s) {
				t.Errorf("stopping for %s: not moving", stop)
			}
			exitR1(t, s)
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
			var svStop, queuedReason string
			systemScan(t, s, `SELECT (SELECT coalesce(stop_reason, '') FROM run_servers WHERE run_id = 'r1'), state_reason FROM runs WHERE id = 'r1'`,
				nil, &svStop, &queuedReason)
			if queuedReason != "auto-resume after "+stop {
				t.Errorf("queued with reason %q", queuedReason)
			}
			if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
				t.Fatal(err)
			}
			var state string
			var placements int
			systemScan(t, s, `SELECT state, (SELECT count(*) FROM placements WHERE run_id = 'r1') FROM runs WHERE id = 'r1'`, nil, &state, &placements)
			_, cached := s.secrets.get("r1")
			if state != StateScheduled || placements != 2 || !cached || svStop != "migrated" {
				t.Fatalf("state %q placements %d secrets cached %v server %q; want scheduled again, secrets kept, server migrated",
					state, placements, cached, svStop)
			}
			if a := assignOf(t, s, 2); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" || a.Resume.SessionID != "sess-1" {
				t.Fatalf("resume assigned %+v, want snapR1 and sess-1", a.Resume)
			}
		})
	}
}

// A transient Run whose host is lost mid-move is placed again, as auto.
func TestTransientInterruptedMoveResumes(t *testing.T) {
	for _, trigger := range []string{"lease", "host"} {
		t.Run(trigger, func(t *testing.T) {
			s, ctx := policyFixture(t, "transient")
			snapshotR1(t, s)
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			reapInterruptedMove(t, s, ctx, trigger)
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
			if state != StateResuming || reason != "auto-resume after preempt" {
				t.Fatalf("Run %s (%s), want resuming", state, reason)
			}
		})
	}
}

// Every requested resume of a transient Run is refused 409 not_resumable,
// and resumability lists never's blocker; nor is it listed resumable.
func TestTransientRefusesResume(t *testing.T) {
	for _, how := range []string{"succeeded", "stopped by request", "running"} {
		t.Run(how, func(t *testing.T) {
			s, ctx := policyFixture(t, "transient")
			switch how {
			case "succeeded":
				exitR1With(t, s, 0)
			case "stopped by request":
				if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
					t.Fatal(err)
				}
				exitR1(t, s)
			}
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
			for _, pctx := range []context.Context{tenantCtx("t1"), operatorCtx()} {
				_, err := s.resumeRun(pctx, &resumeRunInput{RunPath: RunPath{ID: "r1"}})
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_resumable" || he.Message != neverResumableReason {
					t.Fatalf("resume: %v, want 409 not_resumable", err)
				}
			}
			run, err := s.loadRun(context.Background(), "t1", "r1", false)
			if err != nil {
				t.Fatal(err)
			}
			if rs, err := s.resumability(context.Background(), "t1", run); err != nil || !slices.Contains(rs.Blockers, neverResumableReason) {
				t.Fatalf("resumability %+v %v", rs, err)
			}
			out, err := s.listRuns(tenantCtx("t1"), &listRunsInput{Resumable: true})
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(out.Body.Runs, func(r *Run) bool { return r.ID == "r1" }) {
				t.Fatal("listed resumable")
			}
		})
	}
}

// A queued transient Run whose secrets no luxd holds any more ends
// terminated as never does: a resume to supply them would be refused.
func TestSchedulerEndsTransientRunTerminated(t *testing.T) {
	s, ctx := policyFixture(t, "transient")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming', updated_at = now() - interval '1 hour' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET secrets = '[{"name":"TOKEN"}]' WHERE id = 'r1'`)
	s.secrets.drop("r1")
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "its secrets are no longer held; resumePolicy transient" {
		t.Fatalf("state %q reason %q", state, reason)
	}
}
