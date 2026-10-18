package server

import (
	"context"
	"github.com/marcioapm/lux/internal/spec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

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
			execSQL(t, s, ctx, `INSERT INTO git_checkpoint_refs(tenant_id,run_id,repo,checkpoint_id) VALUES('t1','ra','copy','checkpoint-one')`)
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
}
