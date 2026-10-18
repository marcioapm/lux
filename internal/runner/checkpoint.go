package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

func (p *placement) checkpointLimit() int64 {
	if p.assign != nil && p.assign.CheckpointBytes > 0 {
		return p.assign.CheckpointBytes
	}
	return proto.DefaultCheckpointBytes
}

func (p *placement) checkpointRunning(ctx context.Context) {
	if p.assign == nil || p.assign.Spec.Git == nil {
		return
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return
	}
	dir := "checkpoint-live-" + ids.New("stage")
	path := rt + "/" + dir
	if os.Mkdir(path, 0o700) != nil {
		return
	}
	if os.Chown(path, p.user.UID, p.user.GID) != nil {
		return
	}
	a := proto.CheckpointArgs{Dir: proto.ShimRunDir + "/" + dir, MaxBytes: p.checkpointLimit()}
	for _, repo := range p.assign.Spec.Git.Repositories {
		a.Repos = append(a.Repos, proto.CheckpointRepo{Name: repo.Name, Path: repo.Path})
	}
	out, err := p.r.execShim(ctx, p.runID, fmt.Sprintf("%d:%d", p.user.UID, p.user.GID), "checkpoint", a)
	if err != nil {
		p.event(ctx, proto.EvWarning, map[string]any{"message": "git checkpoint: " + err.Error()})
		return
	}
	// This directory was created by the runner and the manifest is replaced,
	// never opened through a link planted by the workload.
	root, err := os.OpenRoot(path)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove("results.json")
	f, err := root.OpenFile("results.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, err = f.Write(out)
	f.Close()
	if err != nil {
		return
	}
	p.collectCheckpointDir(ctx, dir)
}

func (p *placement) collectCheckpoints(ctx context.Context) {
	p.collectCheckpointDir(ctx, fmt.Sprintf("checkpoints-%d", p.epoch))
}

func (p *placement) collectCheckpointDir(ctx context.Context, dir string) {
	if p.assign == nil || p.assign.Spec.Git == nil || len(p.assign.Spec.Git.Repositories) == 0 {
		return
	}
	warn := func(err error) {
		p.event(ctx, proto.EvWarning, map[string]any{"message": "git checkpoint: " + err.Error()})
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		warn(err)
		return
	}
	root, err := os.OpenRoot(rt)
	if err != nil {
		warn(err)
		return
	}
	defer root.Close()
	f, err := root.OpenFile(dir+"/results.json", os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		warn(fmt.Errorf("no completed checkpoint stage: %w", err))
		return
	}
	var results []proto.StagedCheckpoint
	err = json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&results)
	f.Close()
	if err != nil {
		warn(err)
		return
	}
	for i, repo := range p.assign.Spec.Git.Repositories {
		if i >= len(results) || results[i].Repo != repo.Name {
			warn(fmt.Errorf("%s: missing stage result", repo.Name))
			continue
		}
		res := results[i]
		if res.Unchanged {
			continue
		}
		if res.Error != "" {
			warn(fmt.Errorf("%s: %s", repo.Name, res.Error))
			continue
		}
		if res.File != strconv.Itoa(i)+".bundle" || res.Head == "" || res.Refs["HEAD"] != res.Head {
			warn(fmt.Errorf("%s: invalid stage result", repo.Name))
			continue
		}
		src, err := root.OpenFile(dir+"/"+res.File, os.O_RDONLY|oNoFollow, 0)
		if err != nil {
			warn(err)
			continue
		}
		fi, err := src.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != res.Size || res.Size > p.checkpointLimit() {
			src.Close()
			warn(fmt.Errorf("%s: invalid bundle size or type", repo.Name))
			continue
		}
		id := ids.New(ids.Blob)
		size, sum, err := p.r.writeBlob(id, func(w io.Writer) error {
			_, err := io.Copy(w, io.LimitReader(src, p.checkpointLimit()+1))
			return err
		})
		src.Close()
		if err != nil {
			warn(err)
			continue
		}
		ready := &proto.CheckpointReady{Blob: proto.BlobInfo{BlobID: id, Size: size, SHA256: sum}, StagedCheckpoint: res}
		rec := &snapshotRecord{RunID: p.runID, Epoch: p.epoch, Created: time.Now().UnixMilli(), Checkpoint: ready, Uploads: []pendingUpload{{BlobID: id, Path: p.r.blobPath(id), Size: size}}}
		if err := p.r.saveSnapshotRecord(id, rec); err != nil {
			warn(err)
			continue
		}
		ack, err := p.reportAck(ctx, proto.MsgCheckpointReady, ready)
		if err != nil {
			warn(err)
			continue
		}
		if ack.Refused {
			removeSnapshotFiles(p.r, id, rec)
			continue
		}
		p.r.updateRecord(id, func(r *snapshotRecord) { r.Reported = true })
		p.r.uploads.kick()
	}
}
