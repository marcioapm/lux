package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// supersededFixture: rb (t2) stopped at epoch 2 with snapB (epoch 1) and
// its current snapB2 (epoch 2), every blob in S3 under <run>/<blob id>,
// snapB uploaded and snapB2 not (yet). ra (t1) has one snapshot, snapA.
func supersededFixture(t *testing.T) (*Server, context.Context, *fakeS3) {
	t.Helper()
	s, ctx := reportFixture(t)
	f := useFakeS3(t, s)
	if fr := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); fr.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", fr.Type, fr.Data)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pb1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb2', 't2', 'rb', 'hb', 2, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'rb'`)
	if fr := reportSnapshot(t, s, "hb", "rb", 2, snapshotB("snapB2", 2)); fr.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", fr.Type, fr.Data)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id IN ('ra', 'rb')`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = (id <> 'snapB2')`)
	return s, ctx, f
}

func snapshotAvailable(t *testing.T, s *Server, id string) bool {
	t.Helper()
	var ok bool
	systemScan(t, s, `SELECT available FROM snapshots WHERE id = $1`, []any{id}, &ok)
	return ok
}

// blobLocations maps each of run's blobs to its location.
func blobLocations(t *testing.T, s *Server, run string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, location FROM blobs WHERE run_id = $1`, run)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, loc string
			if err := rows.Scan(&id, &loc); err != nil {
				return err
			}
			out[id] = loc
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Older snapshots of a resumable Run stay while the current one is not
// uploaded; once it is, their volumes are deleted (S3 too) and they are
// unavailable. The current one, the older one's output and artifacts, and
// a Run with only its current snapshot are untouched.
func TestReapSuperseded(t *testing.T) {
	for _, state := range []string{StateStopped, StateLost, StateFailed} {
		t.Run(state, func(t *testing.T) {
			s, ctx, f := supersededFixture(t)
			execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'rb'`, state)
			if err := s.reapSuperseded(ctx); err != nil {
				t.Fatal(err)
			}
			if !snapshotAvailable(t, s, "snapB") || len(f.Deleted()) != 0 {
				t.Fatalf("current not uploaded: snapB available %v, deleted %v", snapshotAvailable(t, s, "snapB"), f.Deleted())
			}
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapB2'`)
			if err := s.reapSuperseded(ctx); err != nil {
				t.Fatal(err)
			}
			if snapshotAvailable(t, s, "snapB") || !snapshotAvailable(t, s, "snapB2") || !snapshotAvailable(t, s, "snapA") {
				t.Fatalf("available: snapB %v snapB2 %v snapA %v, want false true true",
					snapshotAvailable(t, s, "snapB"), snapshotAvailable(t, s, "snapB2"), snapshotAvailable(t, s, "snapA"))
			}
			locs := blobLocations(t, s, "rb")
			for id, want := range map[string]string{"bB-vol-snapB": "deleted", "bB-out-snapB": "s3", "bB-art-snapB": "s3",
				"bB-vol-snapB2": "s3", "bB-out-snapB2": "s3", "bB-art-snapB2": "s3"} {
				if locs[id] != want {
					t.Errorf("%s: %s, want %s", id, locs[id], want)
				}
			}
			if got := f.Deleted(); !slices.Equal(got, []string{"rb/bB-vol-snapB"}) {
				t.Fatalf("S3 deletes %v, want rb/bB-vol-snapB", got)
			}
			// Nothing more on the next pass.
			if err := s.reapSuperseded(ctx); err != nil {
				t.Fatal(err)
			}
			if got := f.Deleted(); len(got) != 1 {
				t.Fatalf("second pass deleted again: %v", got)
			}
		})
	}
}

// An older snapshot with a volume still on its host (mid-upload) is left,
// available, for a later pass; a volume the current manifest names too is
// never deleted.
func TestReapSupersededSparesHostAndSharedBlobs(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host' WHERE id = 'bB-vol-snapB'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if !snapshotAvailable(t, s, "snapB") || len(f.Deleted()) != 0 {
		t.Fatalf("mid-upload: snapB available %v, deleted %v", snapshotAvailable(t, s, "snapB"), f.Deleted())
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3' WHERE id = 'bB-vol-snapB'`)
	execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes}', manifest->'volumes' || '[{"name":"x","blobId":"bB-vol-snapB"}]')
		WHERE id = 'snapB2'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshotAvailable(t, s, "snapB") || blobLocations(t, s, "rb")["bB-vol-snapB"] != "s3" || len(f.Deleted()) != 0 {
		t.Fatalf("shared: snapB available %v, blob %s, deleted %v", snapshotAvailable(t, s, "snapB"),
			blobLocations(t, s, "rb")["bB-vol-snapB"], f.Deleted())
	}
}

// Runs whose older snapshot still has a volume on its host are not
// candidates: twenty of them, with lower ids, do not keep a ready Run out
// of the pass's batch.
func TestReapSupersededSkipsBlockedRuns(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, snapshots_superseded)
		SELECT 'ra' || lpad(i::text, 2, '0'), 't2', '{}', 'stopped', 2, true FROM generate_series(0, 19) i`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		SELECT r.id || '-p' || e, 't2', r.id, 'hb', e, 'exited' FROM runs r, generate_series(1, 2) e WHERE r.id LIKE 'ra__'`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, host_id, s3_key)
		SELECT r.id || '-vol' || e, 't2', r.id, e, 'volume', 'work', CASE e WHEN 1 THEN 'host' ELSE 's3' END, 'hb',
			CASE e WHEN 2 THEN r.id || '/' || r.id || '-vol2' END
		FROM runs r, generate_series(1, 2) e WHERE r.id LIKE 'ra__'`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		SELECT r.id || '-s' || e, 't2', r.id, r.id || '-p' || e, e,
			jsonb_build_object('volumes', jsonb_build_array(jsonb_build_object('name', 'work', 'blobId', r.id || '-vol' || e))), 'hb', e = 2
		FROM runs r, generate_series(1, 2) e WHERE r.id LIKE 'ra__'`)
	execSQL(t, s, ctx, `UPDATE runs SET snapshot_id = id || '-s2' WHERE id LIKE 'ra__'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshotAvailable(t, s, "snapB") || !slices.Equal(f.Deleted(), []string{"rb/bB-vol-snapB"}) {
		t.Fatalf("rb not reaped behind blocked Runs: snapB available %v, S3 deletes %v", snapshotAvailable(t, s, "snapB"), f.Deleted())
	}
	var blocked int
	systemScan(t, s, `SELECT count(*) FROM snapshots WHERE run_id LIKE 'ra__' AND available`, nil, &blocked)
	if blocked != 40 {
		t.Fatalf("blocked Runs' available snapshots: %d, want 40", blocked)
	}
}

// A resume --from-snapshot of the older snapshot that commits before the
// reaper's pass makes it the current one: the reaper deletes the formerly
// current one instead. One the reaper claims first is refused (409
// snapshot_unavailable) and the Run keeps its current snapshot. Either
// way the Run never points at a deleted snapshot.
func TestReapSupersededRacesResume(t *testing.T) {
	t.Run("resume first", func(t *testing.T) {
		s, ctx, f := supersededFixture(t)
		execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
		s.secrets.put("rb", map[string]string{})
		locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'rb' FOR UPDATE`); err != nil {
					return err
				}
				close(locked)
				<-release
				if _, err := tx.Exec(ctx, `UPDATE runs SET snapshot_id = 'snapB' WHERE id = 'rb'`); err != nil {
					return err
				}
				return s.requestResume(ctx, tx, "t2", "rb", nil, "resume requested")
			})
		}()
		<-locked
		reaped := make(chan error, 1)
		go func() { reaped <- s.reapSuperseded(ctx) }()
		select {
		case err := <-reaped:
			reaped <- err
		case <-time.After(time.Second):
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-reaped; err != nil {
			t.Fatal(err)
		}
		if err := s.reapSuperseded(ctx); err != nil {
			t.Fatal(err)
		}
		if !snapshotAvailable(t, s, "snapB") || snapshotAvailable(t, s, "snapB2") {
			t.Fatalf("snapB available %v (restoring from it), snapB2 %v (superseded)", snapshotAvailable(t, s, "snapB"), snapshotAvailable(t, s, "snapB2"))
		}
		if got := f.Deleted(); !slices.Equal(got, []string{"rb/bB-vol-snapB2"}) {
			t.Fatalf("S3 deletes %v, want only rb/bB-vol-snapB2", got)
		}
		// The scheduler restores what it was resumed from.
		m, err := restoreManifestOf(t, s, "rb")
		if err != nil || m != "snapB" {
			t.Fatalf("restores %q (%v), want snapB", m, err)
		}
	})
	t.Run("reaper first", func(t *testing.T) {
		s, ctx, _ := supersededFixture(t)
		execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
		if err := s.reapSuperseded(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := s.resumeRun(asTenant(ctx, "t2"), &resumeRunInput{RunPath: RunPath{ID: "rb"}, Body: &resumeRequest{FromSnapshot: "snapB"}})
		if err == nil || !strings.Contains(err.Error(), "no longer available") {
			t.Fatalf("resume from the deleted snapshot: %v, want snapshot_unavailable", err)
		}
		var state, snap string
		systemScan(t, s, `SELECT state, snapshot_id FROM runs WHERE id = 'rb'`, nil, &state, &snap)
		if state != StateStopped || snap != "snapB2" {
			t.Fatalf("rb: %s on %s, want stopped on snapB2", state, snap)
		}
	})
}

// restoreManifestOf is the snapshot the scheduler would restore for run.
func restoreManifestOf(t *testing.T, s *Server, run string) (string, error) {
	t.Helper()
	ctx := context.Background()
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var snap string
		if err := tx.QueryRow(ctx, `SELECT snapshot_id FROM runs WHERE id = $1`, run).Scan(&snap); err != nil {
			return err
		}
		m, err := restoreManifest(ctx, tx, run, snap)
		if err != nil {
			return err
		}
		var available bool
		if err := tx.QueryRow(ctx, `SELECT available FROM snapshots WHERE id = $1`, snap).Scan(&available); err != nil || !available {
			return err
		}
		id = m.SnapshotID
		return nil
	})
	return id, err
}

// A new snapshot report while the reaper holds the Run: it lands after the
// claim, and is not uploaded, so the snapshot that was current stays.
func TestReapSupersededRacesNewSnapshot(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb3', 't2', 'rb', 'hb', 3, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3, state = 'stopping' WHERE id = 'rb'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if fr := reportSnapshot(t, s, "hb", "rb", 3, snapshotB("snapB3", 3)); fr.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", fr.Type, fr.Data)
	}
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if !snapshotAvailable(t, s, "snapB2") || !snapshotAvailable(t, s, "snapB3") || snapshotAvailable(t, s, "snapB") {
		t.Fatalf("available: snapB %v snapB2 %v snapB3 %v, want false true true",
			snapshotAvailable(t, s, "snapB"), snapshotAvailable(t, s, "snapB2"), snapshotAvailable(t, s, "snapB3"))
	}
	if got := f.Deleted(); !slices.Equal(got, []string{"rb/bB-vol-snapB"}) {
		t.Fatalf("S3 deletes %v", got)
	}
}

// A Run is looked at again only once it may have a superseded snapshot:
// the pass that leaves it none clears its flag, a resume --from-snapshot
// sets it, and the formerly current snapshot is then deleted.
func TestReapSupersededFlag(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	flag := func() bool {
		var b bool
		systemScan(t, s, `SELECT snapshots_superseded FROM runs WHERE id = 'rb'`, nil, &b)
		return b
	}
	if !flag() {
		t.Fatal("a second snapshot did not flag rb")
	}
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if flag() {
		t.Fatal("flag kept with nothing left to delete")
	}
	// A stale flag-less Run is not looked at: nothing more is deleted.
	execSQL(t, s, ctx, `UPDATE snapshots SET available = true WHERE id = 'snapB'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.Deleted()) != 1 {
		t.Fatalf("deleted %v without the flag", f.Deleted())
	}
	s.secrets.put("rb", map[string]string{})
	if _, err := s.resumeRun(asTenant(ctx, "t2"), &resumeRunInput{RunPath: RunPath{ID: "rb"}, Body: &resumeRequest{FromSnapshot: "snapB"}}); err != nil {
		t.Fatal(err)
	}
	if !flag() {
		t.Fatal("resume --from-snapshot did not flag rb")
	}
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if !snapshotAvailable(t, s, "snapB") || snapshotAvailable(t, s, "snapB2") {
		t.Fatalf("after resume from snapB: snapB %v snapB2 %v", snapshotAvailable(t, s, "snapB"), snapshotAvailable(t, s, "snapB2"))
	}
}

// A late report from an older placement (its host back after the Run moved
// on) is not the Run's snapshot: once the current one is uploaded, it goes.
func TestReapSupersededLateReport(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb3', 't2', 'rb', 'hb', 3, 'exited')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 4 WHERE id = 'rb'`)
	if fr := reportSnapshot(t, s, "hb", "rb", 3, snapshotB("snapLate", 3)); fr.Type != proto.MsgAck {
		t.Fatalf("late report: %s %s", fr.Type, fr.Data)
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id WHERE id = 'bB-vol-snapLate'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshotAvailable(t, s, "snapLate") || !snapshotAvailable(t, s, "snapB2") {
		t.Fatalf("snapLate %v snapB2 %v, want false true", snapshotAvailable(t, s, "snapLate"), snapshotAvailable(t, s, "snapB2"))
	}
	if got := f.Deleted(); !slices.Contains(got, "rb/bB-vol-snapLate") {
		t.Fatalf("S3 deletes %v", got)
	}
}

// An S3 delete that fails leaves an orphan, logged; the claim stands and
// the Run still has its current snapshot.
func TestReapSupersededS3Failure(t *testing.T) {
	s, ctx, f := supersededFixture(t)
	log := captureLog(s)
	f.failPrefix = "rb/"
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "object orphaned") || !strings.Contains(log.String(), "rb/bB-vol-snapB") {
		t.Fatalf("no orphan logged: %s", log.String())
	}
	if blobLocations(t, s, "rb")["bB-vol-snapB"] != "deleted" || !snapshotAvailable(t, s, "snapB2") {
		t.Fatal("claim lost or current snapshot touched")
	}
	if m, err := restoreManifestOf(t, s, "rb"); err != nil || m != "snapB2" {
		t.Fatalf("rb restores %q (%v), want snapB2", m, err)
	}
}
