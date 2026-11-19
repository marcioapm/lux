package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

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
	for _, how := range []string{"succeeded", "stopped by request", "running", "stopped", "lost", "legacy succeeded", "failed"} {
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
			if slices.Contains([]string{"stopped", "lost", "legacy succeeded", "failed"}, how) {
				// Defensive policy filtering must not rely on the state filter.
				state := how
				if how == "legacy succeeded" {
					state = "succeeded"
				}
				snapshotR1(t, s)
				execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
				execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'r1'`, state)
			}
			var resumable bool
			systemScan(t, s, `SELECT `+resumableSQL+` FROM runs r WHERE id = 'r1'`, nil, &resumable)
			if resumable {
				t.Error("SQL classified transient Run as resumable")
			}
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

// transientMovedThenEnded: r1 (resumePolicy policy) preempted on h1 with
// snapshot snapR1 (volume b-r1-vol), placed again, then exited 0 on its
// second host with a 0-volume snapshot snapE (output, an artifact). Every
// blob is in fake S3 under <run>/<blob id>.
func transientMovedThenEnded(t *testing.T, policy string) (*Server, context.Context, *fakeS3) {
	t.Helper()
	s, ctx := policyFixture(t, policy)
	f := useFakeS3(t, s)
	if err := moveStops["preempt"](t, s); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	var host string
	systemScan(t, s, `SELECT host_id FROM placements WHERE run_id = 'r1' AND epoch = 2`, nil, &host)
	snapshotR1Empty(t, s, host, 2, "snapE")
	code := 0
	if got := s.handleReport(ctx, host, proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "r1", Epoch: 2,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s %s", got.Type, got.Data)
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id WHERE location = 'host'`)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	return s, ctx, f
}

// Once a transient Run that moved is terminated, the next pass deletes the
// volumes of every snapshot it holds, without waiting for retention: the
// blobs deleted (S3 too), those snapshots unavailable. Its output and
// artifacts stay, and its 0-volume exit snapshot stays available. A volume
// still on its host is not deleted until it has arrived.
func TestReapTransientMixedUpload(t *testing.T) {
	s, ctx, f := transientMovedThenEnded(t, "transient")
	// A fully uploaded snapshot makes this Run enter the UPDATE, while a
	// second snapshot of the SAME Run still has one volume on its host.
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "mixed", RunID: "r1", Epoch: 2,
		Volumes: []proto.VolumeSnapshot{
			{Name: "one", Path: "/one", BlobID: "mixed-s3", Size: 1, SHA256: "one"},
			{Name: "two", Path: "/two", BlobID: "mixed-host", Size: 2, SHA256: "two"},
		}}}
	var host string
	systemScan(t, s, `SELECT host_id FROM placements WHERE run_id = 'r1' AND epoch = 2`, nil, &host)
	if got := reportSnapshot(t, s, host, "r1", 2, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
		t.Fatalf("mixed snapshot: %s %s", got.Type, got.Data)
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id WHERE id = 'mixed-s3'`)
	if err := s.reapTransient(ctx); err != nil {
		t.Fatal(err)
	}
	locs := blobLocations(t, s, "r1")
	if snapshotAvailable(t, s, "snapR1") || locs["b-r1-vol"] != "deleted" {
		t.Error("fully uploaded snapshot not reaped")
	}
	if !snapshotAvailable(t, s, "mixed") || locs["mixed-s3"] != "s3" || locs["mixed-host"] != "host" {
		t.Errorf("mixed snapshot claimed mid-upload: available %v locations %v", snapshotAvailable(t, s, "mixed"), locs)
	}
	if !slices.Equal(f.Deleted(), []string{"r1/b-r1-vol"}) {
		t.Errorf("unexpected deletes %v", f.Deleted())
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id WHERE id = 'mixed-host'`)
	if err := s.reapTransient(ctx); err != nil {
		t.Fatal(err)
	}
	locs = blobLocations(t, s, "r1")
	if snapshotAvailable(t, s, "mixed") || locs["mixed-s3"] != "deleted" || locs["mixed-host"] != "deleted" {
		t.Errorf("completed mixed upload not reaped: %v", locs)
	}
	for _, id := range []string{"b-out-snapE", "b-art-snapE"} {
		if locs[id] != "s3" {
			t.Errorf("non-volume %s touched: %s", id, locs[id])
		}
	}
}

func TestReapTransientVolumes(t *testing.T) {
	s, ctx, f := transientMovedThenEnded(t, "transient")
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
	if state != StateTerminated {
		t.Fatalf("r1 %s, want terminated", state)
	}
	// Mid-upload: nothing goes.
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host' WHERE id = 'b-r1-vol'`)
	if err := s.reapTransient(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("deleted mid-upload: %v", got)
	}
	if !snapshotAvailable(t, s, "snapR1") || blobLocations(t, s, "r1")["b-r1-vol"] != "host" {
		t.Fatal("a snapshot whose volume is still on its host was claimed")
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3' WHERE id = 'b-r1-vol'`)
	if err := s.reapTransient(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := f.Deleted(), []string{"r1/b-r1-vol"}; !slices.Equal(got, want) {
		t.Fatalf("S3 deletes %v, want %v", got, want)
	}
	if snapshotAvailable(t, s, "snapR1") {
		t.Error("snapR1 still available")
	}
	if !snapshotAvailable(t, s, "snapE") {
		t.Error("the 0-volume exit snapshot made unavailable")
	}
	locs := blobLocations(t, s, "r1")
	if locs["b-r1-vol"] != "deleted" {
		t.Errorf("b-r1-vol %s, want deleted", locs["b-r1-vol"])
	}
	for _, id := range []string{"b-out-snapE", "b-art-snapE"} {
		if locs[id] != "s3" {
			t.Errorf("%s %s, want s3", id, locs[id])
		}
	}
	var deletedAt bool
	systemScan(t, s, `SELECT deleted_at IS NOT NULL FROM blobs WHERE id = 'b-r1-vol'`, nil, &deletedAt)
	if !deletedAt {
		t.Error("deleted_at not set")
	}
	// Nothing left: a further pass deletes nothing.
	if err := s.reapTransient(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 1 {
		t.Fatalf("a second pass deleted %v", got)
	}
}

// The pass touches no other Run: an auto Run terminated after a move keeps
// its snapshot for retention, a transient Run not terminated (resumed after
// its move) keeps it to resume from, and a transient Run whose only
// snapshot has no volume has nothing to delete.
func TestReapTransientSparesOthers(t *testing.T) {
	t.Run("auto terminated", func(t *testing.T) {
		s, ctx, f := transientMovedThenEnded(t, "auto")
		if _, err := s.terminateRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
			t.Fatal(err)
		}
		if err := s.reapTransient(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.Deleted(); len(got) != 0 || !snapshotAvailable(t, s, "snapR1") {
			t.Fatalf("deleted %v, snapR1 available %v", got, snapshotAvailable(t, s, "snapR1"))
		}
	})
	t.Run("transient resuming", func(t *testing.T) {
		s, ctx := policyFixture(t, "transient")
		f := useFakeS3(t, s)
		if err := moveStops["preempt"](t, s); err != nil {
			t.Fatal(err)
		}
		exitR1(t, s)
		execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)
		if err := s.reapTransient(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.Deleted(); len(got) != 0 || !snapshotAvailable(t, s, "snapR1") {
			t.Fatalf("deleted %v, snapR1 available %v", got, snapshotAvailable(t, s, "snapR1"))
		}
	})
	t.Run("transient, 0-volume snapshot only", func(t *testing.T) {
		s, ctx := policyFixture(t, "transient")
		f := useFakeS3(t, s)
		snapshotR1Empty(t, s, "h1", 1, "snapE")
		reportR1Exit(t, s, 0, "exited")
		execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)
		if err := s.reapTransient(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.Deleted(); len(got) != 0 || !snapshotAvailable(t, s, "snapE") {
			t.Fatalf("deleted %v, snapE available %v", got, snapshotAvailable(t, s, "snapE"))
		}
	})
}

// The reaper's storage tick runs the transient pass.
func TestReapOnceReapsTransient(t *testing.T) {
	s, ctx, f := transientMovedThenEnded(t, "transient")
	var last time.Time
	s.reapOnce(ctx, &last)
	if got := f.Deleted(); !slices.Equal(got, []string{"r1/b-r1-vol"}) {
		t.Fatalf("S3 deletes %v", got)
	}
}
