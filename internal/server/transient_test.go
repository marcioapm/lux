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
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: snapID, RunID: "r1", Epoch: epoch, SessionID: "sess-1",
		MoveStop: requestedMove(t, s, epoch), Volumes: []proto.VolumeSnapshot{}},
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
func TestTransientSnapshotBeforeMove(t *testing.T) {
	for _, stop := range []string{"drain", "preempt", "migrate"} {
		for _, outcome := range []string{"success", "failure", "lease", "host"} {
			t.Run(stop+"/"+outcome, func(t *testing.T) {
				s, ctx := policyFixture(t, "transient")
				snapshotR1Empty(t, s, "h1", 1, "natural")
				if err := moveStops[stop](t, s); err != nil {
					t.Fatal(err)
				}
				want := "succeeded; resumePolicy transient"
				switch outcome {
				case "success":
					reportR1Exit(t, s, 0, "exited")
				case "failure":
					reportR1Exit(t, s, 1, "exited")
					want = "exit code 1; resumePolicy transient"
				case "lease":
					reapInterruptedMove(t, s, ctx, outcome)
					want = "lease expired: host stopped heartbeating; resumePolicy transient"
				case "host":
					reapInterruptedMove(t, s, ctx, outcome)
					want = "host lost: missed heartbeats; resumePolicy transient"
				}
				var state, reason string
				systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
				if state != StateTerminated || reason != want {
					t.Fatalf("natural snapshot followed by %s: %s (%s), want terminated (%s)", stop, state, reason, want)
				}
			})
		}
	}
}

func TestTransientMoveSnapshotProvenance(t *testing.T) {
	for _, stop := range []string{"drain", "preempt", "migrate"} {
		for _, c := range []struct {
			name string
			vols bool
			host bool
		}{{"full volumes", true, false}, {"no declared state volumes", false, false},
			{"full volumes host lost", true, true}, {"no declared state volumes host lost", false, true}} {
			t.Run(stop+"/"+c.name, func(t *testing.T) {
				s, ctx := policyFixture(t, "transient")
				if err := moveStops[stop](t, s); err != nil {
					t.Fatal(err)
				}
				snap := "snapR1"
				if c.vols {
					snapshotR1(t, s)
				} else {
					snap = "move-empty"
					snapshotR1Empty(t, s, "h1", 1, snap)
				}
				execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
				if c.host {
					reapInterruptedMove(t, s, ctx, "lease")
				} else {
					reportR1Exit(t, s, 143, "stopped")
				}
				var state, reason string
				systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
				if state != StateResuming || reason != "auto-resume after "+stop {
					t.Fatalf("genuine move: %s (%s), want resuming", state, reason)
				}
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				if a := assignOf(t, s, 2); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != snap || a.Resume.SessionID != "sess-1" {
					t.Fatalf("resume assigned %+v, want %s and sess-1", a.Resume, snap)
				}
			})
		}
	}
}

// A moved epoch's snapshot proves only that epoch's move. If the next
// epoch completes and a move races its exit snapshot, it ends terminated.
func TestTransientOlderMoveSnapshotIsNotProof(t *testing.T) {
	s, ctx := policyFixture(t, "transient")
	if err := moveStops["preempt"](t, s); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	var host string
	systemScan(t, s, `SELECT host_id FROM placements WHERE run_id = 'r1' AND epoch = 2`, nil, &host)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running', started_at = now() WHERE run_id = 'r1' AND epoch = 2`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET stop_reason = 'drain', stop_requested_at = now() WHERE run_id = 'r1' AND epoch = 2`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)
	code := 0
	if got := s.handleReport(ctx, host, proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "r1", Epoch: 2,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s %s", got.Type, got.Data)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "succeeded; resumePolicy transient" {
		t.Fatalf("older move snapshot reused: %s (%s)", state, reason)
	}
}

// moveHost stops r1's placement on host the way each move does. A migrate
// is refused by the API until the Run runs, so it is recorded as
// migrateRun records it.
func moveHost(t *testing.T, s *Server, stop, host string) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch stop {
	case "preempt":
		err = s.hostEvicting(ctx, host, proto.Evicting{Reason: "spot"})
	case "drain":
		_, err = s.drainHost(operatorCtx(), &drainHostInput{HostPath: HostPath{ID: host}, Body: &drainHostRequest{ForceEvict: true}})
	case "migrate":
		err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := s.requestStop(ctx, tx, "t1", "r1", "migrate")
			return err
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	systemScan(t, s, `SELECT coalesce(p.stop_reason, '') FROM placements p JOIN runs r ON r.id = p.run_id AND p.epoch = r.current_epoch
		WHERE r.id = 'r1'`, nil, &reason)
	if reason != stop {
		t.Fatalf("placement stop reason %q, want %q", reason, stop)
	}
}

// startingR1 reports r1's placement epoch on host as starting, its image
// pulled: not yet its volumes or its container.
func startingR1(t *testing.T, s *Server, host string, epoch int) {
	t.Helper()
	ms := time.Now().UnixMilli()
	if got := s.handleReport(context.Background(), host, proto.Frame{Type: proto.MsgStatus, ID: 3, RunID: "r1", Epoch: epoch,
		Data: proto.Marshal(proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms}})}); got.Type != proto.MsgAck {
		t.Fatalf("starting report: %s %s", got.Type, got.Data)
	}
}

// endBeforeStart ends r1's starting placement epoch on host: its runner's
// "stopped before start" report (finishWithoutContainer: no snapshot), its
// lease lost, or its host lost (other, a host that keeps heartbeating).
func endBeforeStart(t *testing.T, s *Server, ctx context.Context, how, host, other string, epoch int) {
	t.Helper()
	var err error
	switch how {
	case "exit":
		code, ms := 0, time.Now().UnixMilli()
		if got := s.handleReport(ctx, host, proto.Frame{Type: proto.MsgStatus, ID: 4, RunID: "r1", Epoch: epoch,
			Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped", Message: "stopped before start",
				Times: map[string]int64{"imageReady": ms}})}); got.Type != proto.MsgAck {
			t.Fatalf("exit report: %s %s", got.Type, got.Data)
		}
	case "lease":
		execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 minute' WHERE run_id = 'r1' AND epoch = $1`, epoch)
		err = s.reapLeases(ctx)
	case "host":
		execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = $1`, host)
		execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() WHERE id = $1`, other)
		err = s.reapHosts(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM placements WHERE run_id = 'r1' AND epoch = $1`, []any{epoch}, &state)
	if want := map[string]string{"exit": "exited", "lease": "lost", "host": "lost"}[how]; state != want {
		t.Fatalf("placement %s, want %s", state, want)
	}
}

// A move that cancels a transient placement's start (image pull, volume
// restore, clone) leaves no snapshot of that epoch: nothing ran in it, so
// it is not a natural end. The Run is placed again from the snapshot it
// already had (none on its first placement), as auto, and that snapshot is
// kept, never reaped as a terminated Run's.
func TestTransientMoveBeforeStartResumes(t *testing.T) {
	for _, stop := range []string{"drain", "preempt", "migrate"} {
		for _, how := range []string{"exit", "lease", "host"} {
			t.Run(stop+"/"+how+"/first placement", func(t *testing.T) {
				s, ctx := policyFixture(t, "transient")
				execSQL(t, s, ctx, `UPDATE placements SET state = 'assigned', container_started_at = NULL, started_at = NULL WHERE id = 'p1'`)
				execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled' WHERE id = 'r1'`)
				startingR1(t, s, "h1", 1)
				moveHost(t, s, stop, "h1")
				endBeforeStart(t, s, ctx, how, "h1", "h2", 1)
				var state, reason string
				systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
				if state != StateResuming || reason != "auto-resume after "+stop {
					t.Fatalf("Run %s (%s), want resuming after %s", state, reason, stop)
				}
				readyHost(t, s, "h3", "default", "", false)
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				if a := assignOf(t, s, 2); a.RunID != "r1" || a.Resume != nil && a.Resume.Snapshot != nil {
					t.Fatalf("placed again with %+v, want no snapshot", a)
				}
			})
			t.Run(stop+"/"+how+"/resumed placement", func(t *testing.T) {
				s, ctx := policyFixture(t, "transient")
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
				if a := assignOf(t, s, 2); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" {
					t.Fatalf("epoch 2 assigned %+v, want snapR1", a.Resume)
				}
				var host string
				systemScan(t, s, `SELECT host_id FROM placements WHERE run_id = 'r1' AND epoch = 2`, nil, &host)
				startingR1(t, s, host, 2)
				moveHost(t, s, stop, host)
				readyHost(t, s, "h3", "default", "", false)
				endBeforeStart(t, s, ctx, how, host, "h3", 2)
				var state, reason, snap string
				systemScan(t, s, `SELECT state, state_reason, coalesce(snapshot_id, '') FROM runs WHERE id = 'r1'`, nil, &state, &reason, &snap)
				if state != StateResuming || reason != "auto-resume after "+stop || snap != "snapR1" {
					t.Fatalf("Run %s (%s) snapshot %q, want resuming after %s from snapR1", state, reason, snap, stop)
				}
				if err := s.reapTransient(ctx); err != nil {
					t.Fatal(err)
				}
				if !snapshotAvailable(t, s, "snapR1") || blobLocations(t, s, "r1")["b-r1-vol"] != "s3" || len(f.Deleted()) != 0 {
					t.Fatalf("snapR1 reaped: available %v, locations %v, deleted %v",
						snapshotAvailable(t, s, "snapR1"), blobLocations(t, s, "r1"), f.Deleted())
				}
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				if a := assignOf(t, s, 3); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" || a.Resume.SessionID != "sess-1" {
					t.Fatalf("epoch 3 assigned %+v, want snapR1 and sess-1", a.Resume)
				}
			})
		}
	}
}

// A container that started unheard by luxd (a shim start failed with no
// running report, or its in-flight containerStarted dropped), then exited
// naturally, proves its start with its same-epoch exit snapshot (no
// moveStop). A move recorded after it, then the lease or host lost before
// the end report, ends the Run terminated, never resumed from that snapshot.
func TestTransientSnapshotProvesStartedWhenStartUnheard(t *testing.T) {
	for _, stop := range []string{"drain", "preempt", "migrate"} {
		for _, how := range []string{"lease", "host"} {
			t.Run(stop+"/"+how, func(t *testing.T) {
				s, ctx := policyFixture(t, "transient")
				execSQL(t, s, ctx, `UPDATE placements SET state = 'assigned', container_started_at = NULL, started_at = NULL WHERE id = 'p1'`)
				execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled' WHERE id = 'r1'`)
				startingR1(t, s, "h1", 1)
				snapshotR1Empty(t, s, "h1", 1, "natural")
				var marked bool
				systemScan(t, s, `SELECT container_started_at IS NOT NULL OR started_at IS NOT NULL FROM placements WHERE id = 'p1'`, nil, &marked)
				if marked {
					t.Fatal("placement marked started: the start must be unheard")
				}
				moveHost(t, s, stop, "h1")
				reapInterruptedMove(t, s, ctx, how)
				want := map[string]string{"lease": "lease expired: host stopped heartbeating", "host": "host lost: missed heartbeats"}[how] +
					"; resumePolicy transient"
				var state, reason string
				var placements int
				systemScan(t, s, `SELECT state, state_reason, (SELECT count(*) FROM placements WHERE run_id = 'r1') FROM runs WHERE id = 'r1'`,
					nil, &state, &reason, &placements)
				if state != StateTerminated || reason != want {
					t.Fatalf("Run %s (%s), want terminated (%s)", state, reason, want)
				}
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				systemScan(t, s, `SELECT count(*) FROM placements WHERE run_id = 'r1'`, nil, &placements)
				if placements != 1 {
					t.Fatalf("%d placements, want 1: resumed", placements)
				}
			})
		}
	}
}

// A container whose start luxd hears only from the end report's
// containerStarted time (no running report arrived, so started_at is NULL)
// and no snapshot of its epoch, exited naturally; a move luxd recorded
// before that end report arrived is not a move of this exit: the Run ends
// terminated, not placed again as a start the move cancelled.
func TestTransientContainerStartedMarkProvesStart(t *testing.T) {
	for _, stop := range []string{"drain", "preempt", "migrate"} {
		t.Run(stop, func(t *testing.T) {
			s, ctx := policyFixture(t, "transient")
			execSQL(t, s, ctx, `UPDATE placements SET state = 'assigned', container_started_at = NULL, started_at = NULL WHERE id = 'p1'`)
			execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled' WHERE id = 'r1'`)
			startingR1(t, s, "h1", 1)
			moveHost(t, s, stop, "h1")
			code, ms := 0, time.Now().UnixMilli()
			if got := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, ID: 4, RunID: "r1", Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited",
					Times: map[string]int64{"imageReady": ms, "containerStarted": ms}})}); got.Type != proto.MsgAck {
				t.Fatalf("exit report: %s %s", got.Type, got.Data)
			}
			var marked, startedAt, snapshot bool
			systemScan(t, s, `SELECT container_started_at IS NOT NULL, started_at IS NOT NULL,
					EXISTS (SELECT 1 FROM snapshots WHERE run_id = 'r1') FROM placements WHERE id = 'p1'`,
				nil, &marked, &startedAt, &snapshot)
			if !marked || startedAt || snapshot {
				t.Fatalf("container_started_at set %v, started_at set %v, snapshot %v: want only the start mark", marked, startedAt, snapshot)
			}
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
			if state != StateTerminated || reason != "succeeded; resumePolicy transient" {
				t.Fatalf("Run %s (%s), want terminated (succeeded; resumePolicy transient)", state, reason)
			}
		})
	}
}

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

// A transient Run whose host is lost after its move export is placed again.
// Lost before that export reaches luxd, there is no proof it can resume.
func TestTransientInterruptedMoveResumes(t *testing.T) {
	for _, trigger := range []string{"lease", "host"} {
		t.Run(trigger, func(t *testing.T) {
			s, ctx := policyFixture(t, "transient")
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			snapshotR1(t, s)
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
			reapInterruptedMove(t, s, ctx, trigger)
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
			if state != StateResuming || reason != "auto-resume after preempt" {
				t.Fatalf("Run %s (%s), want resuming", state, reason)
			}
		})
	}
	t.Run("before export", func(t *testing.T) {
		s, ctx := policyFixture(t, "transient")
		if err := moveStops["preempt"](t, s); err != nil {
			t.Fatal(err)
		}
		reapInterruptedMove(t, s, ctx, "lease")
		var state string
		systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
		if state != StateTerminated {
			t.Fatalf("Run %s, want terminated", state)
		}
	})
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
