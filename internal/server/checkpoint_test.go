package server

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func TestCheckpointReportAndUpload(t *testing.T) {
	s, _ := reportFixture(t)
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
