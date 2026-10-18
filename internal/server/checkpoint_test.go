package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// reportCheckpoint reports checkpoint id of ra's repo api at epoch 1 from
// host ha and fails unless it is accepted.
func reportCheckpoint(t *testing.T, s *Server, id string) {
	t.Helper()
	head := strings.Repeat("a", 40)
	c := proto.CheckpointReady{Blob: proto.BlobInfo{BlobID: id, Size: 10, SHA256: sha("bundle")}, StagedCheckpoint: proto.StagedCheckpoint{Repo: "api", Head: head, Branch: "main", Refs: map[string]string{"HEAD": head, "refs/heads/main": head}, Size: 10}}
	f := s.handleReport(context.Background(), "ha", proto.Frame{Type: proto.MsgCheckpointReady, ID: 1, RunID: "ra", Epoch: 1, Data: proto.Marshal(c)})
	if f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report %s: %+v", id, f)
	}
}

func uploadCheckpoint(s *Server, id string) error {
	return s.blobUploaded(context.Background(), &runnerBlob{id: id, tenantID: "t1", runID: "ra", epoch: 1, kind: "checkpoint"})
}

func blobLocation(t *testing.T, s *Server, id string) string {
	t.Helper()
	var location string
	systemScan(t, s, `SELECT location FROM blobs WHERE id = $1`, []any{id}, &location)
	return location
}

func fromSpec(run, repo string) spec.RunSpec {
	return spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "copy", From: &spec.RepositoryFrom{Run: run, Repo: repo}}}}}
}

// A destination resolving ra's latest checkpoint while ra's next checkpoint
// finishes uploading: the upload's superseded sweep waits for the
// destination's transaction and then sees its pin, so the pinned blob stays.
func TestCheckpointPinRacesSupersede(t *testing.T) {
	s, ctx := reportFixture(t)
	useFakeS3(t, s)
	reportCheckpoint(t, s, "checkpoint-one")
	if err := uploadCheckpoint(s, "checkpoint-one"); err != nil {
		t.Fatal(err)
	}
	reportCheckpoint(t, s, "checkpoint-two")
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,spec,state) VALUES('destination','t1','{}','submitted')`)

	resolved, release := make(chan error, 1), make(chan struct{})
	submitted := make(chan error, 1)
	go func() {
		submitted <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			sp := fromSpec("ra", "api")
			err := resolveCheckpointSources(ctx, tx, "t1", &sp)
			resolved <- err
			if err != nil {
				return err
			}
			<-release
			if sp.Git.Repositories[0].From.Checkpoint != "checkpoint-one" {
				t.Errorf("resolved %q", sp.Git.Repositories[0].From.Checkpoint)
			}
			return pinCheckpointSources(ctx, tx, "t1", "destination", sp)
		})
	}()
	if err := <-resolved; err != nil {
		close(release)
		t.Fatal(err)
	}
	uploaded := make(chan error, 1)
	go func() { uploaded <- uploadCheckpoint(s, "checkpoint-two") }()
	// Both with and without the source-row lock the upload blocks on the
	// submit transaction (the run row, or the blob row in the sweep).
	waitBlocked(t, s)
	close(release)
	if err := <-submitted; err != nil {
		t.Fatal(err)
	}
	if err := <-uploaded; err != nil {
		t.Fatal(err)
	}
	if loc := blobLocation(t, s, "checkpoint-one"); loc != "s3" {
		t.Fatalf("pinned checkpoint-one is %s", loc)
	}
	if loc := blobLocation(t, s, "checkpoint-two"); loc != "s3" {
		t.Fatalf("checkpoint-two is %s", loc)
	}
}

// A checkpoint reported by the previous placement on its host, after luxd
// has moved the Run to the next epoch, is accepted as that placement's
// final state, as a snapshot is; a status from it is stale.
func TestCheckpointFromPreviousEpoch(t *testing.T) {
	s, ctx := reportFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'ra'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pa2', 't1', 'ra', 'hb', 2, 'assigned')`)
	reportCheckpoint(t, s, "checkpoint-old")
	var epoch int
	systemScan(t, s, `SELECT epoch FROM git_checkpoints WHERE id = 'checkpoint-old'`, nil, &epoch)
	if epoch != 1 {
		t.Fatalf("checkpoint epoch %d, want 1", epoch)
	}
	f := s.handleReport(ctx, "ha", proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "ra", Epoch: 1, Data: proto.Marshal(proto.Status{State: "running"})})
	var nack proto.Nack
	if f.Type != proto.MsgNack || json.Unmarshal(f.Data, &nack) != nil || !nack.Stale {
		t.Fatalf("status from epoch 1: %s %s, want a stale nack", f.Type, f.Data)
	}
}

// A checkpoint report whose branch or ref names git would refuse, or read
// as an option, is refused; nothing of it is recorded.
func TestCheckpointRefusesInvalidBranch(t *testing.T) {
	s, ctx := reportFixture(t)
	head := strings.Repeat("a", 40)
	for i, c := range []struct {
		branch string
		refs   map[string]string
	}{
		{"-evil", map[string]string{"HEAD": head}},
		{"--recurse-submodules", map[string]string{"HEAD": head}},
		{"a..b", map[string]string{"HEAD": head}},
		{"main", map[string]string{"HEAD": head, "refs/heads/-x": head}},
		{"main", map[string]string{"HEAD": head, "refs/heads/a b": head}},
		{"main", map[string]string{"HEAD": head, "refs/heads/x.lock": head}},
	} {
		id := "checkpoint-bad-" + strconv.Itoa(i)
		cp := proto.CheckpointReady{Blob: proto.BlobInfo{BlobID: id, Size: 10, SHA256: sha("bundle")},
			StagedCheckpoint: proto.StagedCheckpoint{Repo: "api", Head: head, Branch: c.branch, Refs: c.refs, Size: 10}}
		f := s.handleReport(ctx, "ha", proto.Frame{Type: proto.MsgCheckpointReady, ID: 1, RunID: "ra", Epoch: 1, Data: proto.Marshal(cp)})
		if f.Type != proto.MsgAck || !ackRefused(t, f) {
			t.Errorf("branch %q refs %v: %s %s, want refused", c.branch, c.refs, f.Type, f.Data)
		}
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM git_checkpoints`, nil, &n)
	if n != 0 {
		t.Fatalf("%d checkpoints recorded", n)
	}
	reportCheckpoint(t, s, "checkpoint-good")
}

func TestCheckpointSourceTenantLookup(t *testing.T) {
	s, _ := reportFixture(t)
	ctx := context.Background()
	lookup := func(tenant, source string) error {
		return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			sp := spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "copy", From: &spec.RepositoryFrom{Run: source, Repo: "api"}}}}}
			return resolveCheckpointSources(ctx, tx, tenant, &sp)
		})
	}
	cross := lookup("t2", "ra")
	missing := lookup("t2", "not-a-run")
	if cross == nil || missing == nil {
		t.Fatalf("cross=%v missing=%v", cross, missing)
	}
	if strings.ReplaceAll(cross.Error(), "ra", "SOURCE") != strings.ReplaceAll(missing.Error(), "not-a-run", "SOURCE") {
		t.Fatalf("different errors: %v / %v", cross, missing)
	}
	if err := lookup("t1", "ra"); err == nil || !strings.Contains(err.Error(), "has no checkpoint") {
		t.Fatalf("no checkpoint: %v", err)
	}
}

func TestCheckpointReportAndUpload(t *testing.T) {
	s, _ := reportFixture(t)
	useFakeS3(t, s)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	for _, id := range []string{"checkpoint-one", "checkpoint-two"} {
		c := proto.CheckpointReady{Blob: proto.BlobInfo{BlobID: id, Size: 10, SHA256: sha("bundle")}, StagedCheckpoint: proto.StagedCheckpoint{Repo: "api", Head: head, Branch: "main", Refs: map[string]string{"HEAD": head, "refs/heads/main": head}, Size: 10}}
		f := s.handleReport(ctx, "ha", proto.Frame{Type: proto.MsgCheckpointReady, ID: 1, RunID: "ra", Epoch: 1, Data: proto.Marshal(c)})
		if f.Type != proto.MsgAck || ackRefused(t, f) {
			t.Fatalf("report: %+v", f)
		}
		if err := s.blobUploaded(ctx, &runnerBlob{id: id, tenantID: "t1", runID: "ra", epoch: 1, kind: "checkpoint"}); err != nil {
			t.Fatal(err)
		}
		if id == "checkpoint-one" {
			execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,spec,state) VALUES('destination','t1','{}','submitted')`)
			execSQL(t, s, ctx, `INSERT INTO git_checkpoint_refs(tenant_id,run_id,repo,checkpoint_id) VALUES('t1','destination','copy','checkpoint-one')`)
		}
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var id string
		var n int
		if err := tx.QueryRow(ctx, `SELECT id FROM git_checkpoints WHERE run_id='ra' AND latest AND uploaded`).Scan(&id); err != nil {
			return err
		}
		if id != "checkpoint-two" {
			t.Fatalf("latest: %s", id)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id='ra' AND type='git.checkpoint'`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("events: %d", n)
		}
		var location string
		if err := tx.QueryRow(ctx, `SELECT location FROM blobs WHERE id='checkpoint-one'`).Scan(&location); err != nil {
			return err
		}
		if location != "s3" {
			t.Fatalf("pinned checkpoint deleted: %s", location)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state='terminated',terminated_at=now()-interval '100 days' WHERE id='ra'`)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var location string
		err := tx.QueryRow(ctx, `SELECT location FROM blobs WHERE id='checkpoint-one'`).Scan(&location)
		if err == nil && location != "s3" {
			t.Fatalf("source expiry deleted pinned blob: %s", location)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := checkpointDownload(s, "checkpoint-one", "&run=destination&epoch=1"); code != 404 {
		t.Fatalf("unplaced destination download: %d", code)
	}
	execSQL(t, s, ctx, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state) VALUES('pd','t1','destination','ha',1,'starting')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch=1 WHERE id='destination'`)
	for _, c := range []struct {
		name, id, scope string
		want            int
	}{
		{"pinned destination", "checkpoint-one", "&run=destination&epoch=1", 302},
		{"crafted blob reference", "checkpoint-two", "&run=destination&epoch=1", 404},
		{"wrong run", "checkpoint-one", "&run=ra&epoch=1", 404},
		{"wrong epoch", "checkpoint-one", "&run=destination&epoch=2", 404},
		{"malformed epoch", "checkpoint-one", "&run=destination&epoch=x", 404},
		{"missing epoch", "checkpoint-one", "&run=destination", 404},
		{"missing run", "checkpoint-one", "&epoch=1", 404},
		{"no scope", "checkpoint-one", "", 404},
	} {
		if code := checkpointDownload(s, c.id, c.scope); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
}

// checkpointDownload is the runner's GET for a checkpoint blob, with the
// run/epoch scope runner/git.go appends.
func checkpointDownload(s *Server, id, scope string) int {
	req := httptest.NewRequest(http.MethodGet, "/runner/v1/blobs/"+id+"?host=ha"+scope, nil)
	req.SetPathValue("id", id)
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.wrap(s.serveRunnerBlobDownload)(rec, req)
	return rec.Code
}

func pinOf(t *testing.T, s *Server, run, repo string) string {
	t.Helper()
	var id string
	systemScan(t, s, `SELECT coalesce((SELECT checkpoint_id FROM git_checkpoint_refs WHERE run_id = $1 AND repo = $2), '')`, []any{run, repo}, &id)
	return id
}

// A `from` repository added on resume whose clone fails is dropped with its
// pin: the pinned checkpoint becomes reclaimable once superseded, and adding
// the repository again pins and serves the source's current checkpoint.
func TestResumeAddFromAfterFailedClone(t *testing.T) {
	s, ctx := reportFixture(t)
	useFakeS3(t, s)
	reportCheckpoint(t, s, "checkpoint-one")
	if err := uploadCheckpoint(s, "checkpoint-one"); err != nil {
		t.Fatal(err)
	}
	dst := stoppedWith(t, s, noSecretsSpec())
	add := func(requestID string) {
		t.Helper()
		if _, err := resumeSecrets(tenantCtx("t1"), s, dst, resumeRequest{RequestID: requestID,
			Git: &resumeGit{Repositories: []spec.Repository{{Name: "copy", From: &spec.RepositoryFrom{Run: "ra", Repo: "api"}}}}}); err != nil {
			t.Fatalf("%s: %v", requestID, err)
		}
	}
	add("r-1")
	if pin := pinOf(t, s, dst, "copy"); pin != "checkpoint-one" {
		t.Fatalf("pin after the first add: %q", pin)
	}
	failClone(t, s, dst, "copy", "r-1")
	if pin := pinOf(t, s, dst, "copy"); pin != "" {
		t.Fatalf("pin after the failed clone: %q", pin)
	}
	reportCheckpoint(t, s, "checkpoint-two")
	if err := uploadCheckpoint(s, "checkpoint-two"); err != nil {
		t.Fatal(err)
	}
	if loc := blobLocation(t, s, "checkpoint-one"); loc != "deleted" {
		t.Fatalf("unpinned superseded checkpoint-one: %s, want deleted", loc)
	}
	add("r-2")
	if pin := pinOf(t, s, dst, "copy"); pin != "checkpoint-two" {
		t.Fatalf("pin after the re-add: %q", pin)
	}
	var stored spec.RunSpec
	systemScan(t, s, `SELECT spec FROM runs WHERE id = $1`, []any{dst}, &stored)
	if i := slices.IndexFunc(stored.Git.Repositories, func(r spec.Repository) bool { return r.Name == "copy" }); i < 0 || stored.Git.Repositories[i].From.Checkpoint != "checkpoint-two" {
		t.Fatalf("stored repositories: %+v", stored.Git.Repositories)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running', current_epoch = current_epoch + 1 WHERE id = $1`, dst)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		SELECT 'p-'||id||'-'||current_epoch, 't1', id, 'ha', current_epoch, 'starting', '{}' FROM runs WHERE id = $1`, dst)
	var epoch int
	systemScan(t, s, `SELECT current_epoch FROM runs WHERE id = $1`, []any{dst}, &epoch)
	if code := checkpointDownload(s, "checkpoint-two", "&run="+dst+"&epoch="+strconv.Itoa(epoch)); code != http.StatusFound {
		t.Fatalf("re-added clone's download: %d", code)
	}
}
