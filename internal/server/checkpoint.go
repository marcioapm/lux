package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/apitypes"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
)

type checkpointInput struct {
	RunPath
	Wait bool `query:"wait"`
}

type checkpointOutput struct {
	Body struct {
		Checkpoints []Checkpoint `json:"checkpoints"`
	}
}
type Checkpoint = apitypes.GitCheckpoint

func (s *Server) listCheckpoints(ctx context.Context, in *RunPath) (*checkpointOutput, error) {
	out := &checkpointOutput{}
	out.Body.Checkpoints = []Checkpoint{}
	err := s.db.Tx(ctx, store.Tenant(principal(ctx).TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT c.id,c.repo,c.head,c.branch,c.refs,c.size,c.epoch,c.created_at,c.uploaded FROM git_checkpoints c JOIN blobs b ON b.id=c.id WHERE c.run_id=$1 AND c.latest AND b.location <> 'deleted' ORDER BY c.repo`, in.ID)
		if err != nil {
			return err
		}
		out.Body.Checkpoints, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Checkpoint])
		return err
	})
	return out, err
}

func (s *Server) checkpointRun(ctx context.Context, in *checkpointInput) (*checkpointOutput, error) {
	var host string
	var epoch int
	err := s.db.Tx(ctx, store.Tenant(principal(ctx).TenantID), func(tx pgx.Tx) error {
		var state string
		var sp spec.RunSpec
		if err := tx.QueryRow(ctx, `SELECT state,current_epoch,spec FROM runs WHERE id=$1`, in.ID).Scan(&state, &epoch, &sp); err != nil {
			return err
		}
		if sp.Git == nil || len(sp.Git.Repositories) == 0 {
			return errf(http.StatusConflict, "no_checkpoint", "the Run has no repositories")
		}
		if state != StateRunning {
			return errf(http.StatusConflict, "not_running", "checkpoint needs a running Run")
		}
		var err error
		host, _, err = placementHost(ctx, tx, in.ID, epoch)
		return err
	})
	if err != nil {
		return nil, err
	}
	before, err := s.listCheckpoints(ctx, &in.RunPath)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	if err := s.hub.SendLive(host, proto.Frame{Type: "checkpoint", RunID: in.ID, Epoch: epoch}); err != nil {
		return nil, errf(http.StatusServiceUnavailable, "host_unreachable", "%v", err)
	}
	if !in.Wait {
		return before, nil
	}
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errf(http.StatusGatewayTimeout, "checkpoint_timeout", "checkpoint did not finish uploading in time")
		case <-tick.C:
			out, err := s.listCheckpoints(ctx, &in.RunPath)
			if err != nil {
				return nil, err
			}
			if len(out.Body.Checkpoints) == 0 {
				continue
			}
			ready := true
			for _, c := range out.Body.Checkpoints {
				if c.CreatedAt.Before(started) {
					ready = false
				}
			}
			if ready {
				return out, nil
			}
		}
	}
}

func resolveCheckpointSources(ctx context.Context, tx pgx.Tx, tenant string, sp *spec.RunSpec) error {
	if sp.Git == nil {
		return nil
	}
	for i := range sp.Git.Repositories {
		r := &sp.Git.Repositories[i]
		if r.From == nil {
			continue
		}
		f := r.From
		if f.Checkpoint != "" || f.Head != "" || f.Branch != "" {
			return errf(422, "invalid_spec", "from checkpoint fields are set by luxd")
		}
		var source spec.RunSpec
		err := tx.QueryRow(ctx, `SELECT spec FROM runs WHERE id=$1 AND tenant_id=$2`, f.Run, tenant).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(422, "invalid_spec", "source Run %s does not exist", f.Run)
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT c.id,c.head,c.branch FROM git_checkpoints c JOIN blobs b ON b.id=c.id WHERE c.run_id=$1 AND c.tenant_id=$2 AND c.repo=$3 AND c.latest AND c.uploaded AND b.location='s3' FOR SHARE OF b`, f.Run, tenant, f.Repo).Scan(&f.Checkpoint, &f.Head, &f.Branch)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(422, "invalid_spec", "%s has no checkpoint of repo %s; lux checkpoint %s while it runs", f.Run, f.Repo, f.Run)
		}
		if err != nil {
			return err
		}
		if source.Git != nil {
			for _, sr := range source.Git.Repositories {
				if sr.Name == f.Repo {
					r.URL = sr.URL
				}
			}
		}
	}
	return nil
}

func pinCheckpointSources(ctx context.Context, tx pgx.Tx, tenant, run string, sp spec.RunSpec) error {
	if sp.Git == nil {
		return nil
	}
	for _, r := range sp.Git.Repositories {
		if r.From == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO git_checkpoint_refs(tenant_id,run_id,repo,checkpoint_id) VALUES($1,$2,$3,$4) ON CONFLICT(run_id,repo) DO NOTHING`, tenant, run, r.Name, r.From.Checkpoint); err != nil {
			return err
		}
	}
	return nil
}

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
