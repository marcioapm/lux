package server

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
)

func applyCheckpointReady(ctx context.Context, tx pgx.Tx, tenant, host, run string, epoch int, c proto.CheckpointReady) (bool, error) {
	if c.Repo == "" || c.Head == "" || c.Refs["HEAD"] != c.Head || c.Size < 0 || c.Size > proto.DefaultCheckpointBytes || !storableText(c.Repo, c.Head, c.Branch, c.Blob.BlobID, c.Blob.SHA256) {
		return true, nil
	}
	for ref, sha := range c.Refs {
		if (ref != "HEAD" && !strings.HasPrefix(ref, "refs/heads/")) || !isCommitID(sha) {
			return true, nil
		}
	}
	var old proto.StagedCheckpoint
	err := tx.QueryRow(ctx, `SELECT repo,head,branch,refs,size FROM git_checkpoints WHERE id=$1 AND tenant_id=$2 AND run_id=$3`, c.Blob.BlobID, tenant, run).Scan(&old.Repo, &old.Head, &old.Branch, &old.Refs, &old.Size)
	if err == nil {
		return old.Repo != c.Repo || old.Head != c.Head || !reflect.DeepEqual(old.Refs, c.Refs), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer sp.Rollback(ctx)
	err = insertBlob(ctx, sp, tenant, run, epoch, host, c.Blob.BlobID, "checkpoint", c.Repo, c.Blob.Size, c.Blob.SHA256, "")
	var foreign *foreignBlobError
	if errors.As(err, &foreign) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_, err = sp.Exec(ctx, `INSERT INTO git_checkpoints(id,tenant_id,run_id,repo,head,branch,refs,size,epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.Blob.BlobID, tenant, run, c.Repo, c.Head, c.Branch, c.Refs, c.Size, epoch)
	if err != nil {
		return false, err
	}
	return false, sp.Commit(ctx)
}

func checkpointUploaded(ctx context.Context, tx pgx.Tx, id string) error {
	var tenant, run, repo, head string
	var epoch int
	var size int64
	var refs map[string]string
	err := tx.QueryRow(ctx, `SELECT tenant_id,run_id,repo,head,refs,size,epoch FROM git_checkpoints WHERE id=$1`, id).Scan(&tenant, &run, &repo, &head, &refs, &size, &epoch)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE git_checkpoints SET latest=false WHERE run_id=$1 AND repo=$2 AND latest`, run, repo); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE git_checkpoints SET latest=true,uploaded=true WHERE id=$1`, id); err != nil {
		return err
	}
	return addEvent(ctx, tx, tenant, run, epoch, "git.checkpoint", map[string]any{"repo": repo, "head": head, "refs": refs, "size": size})
}
