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

func (p *placement) collectCheckpoints(ctx context.Context) {
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
	dir := fmt.Sprintf("checkpoints-%d", p.epoch)
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
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != res.Size || res.Size > proto.DefaultCheckpointBytes {
			src.Close()
			warn(fmt.Errorf("%s: invalid bundle size or type", repo.Name))
			continue
		}
		id := ids.New(ids.Blob)
		size, sum, err := p.r.writeBlob(id, func(w io.Writer) error {
			_, err := io.Copy(w, io.LimitReader(src, proto.DefaultCheckpointBytes+1))
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
