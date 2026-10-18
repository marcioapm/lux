package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// stageCheckpointDir writes the stop-time checkpoint stage of run1's repo
// api: results.json naming 0.bundle, which the caller creates.
func stageCheckpointDir(t *testing.T, f *finishFixture) string {
	t.Helper()
	f.p.assign.Spec.Git = &spec.Git{Repositories: []spec.Repository{{Name: "api", Path: "/work/api"}}}
	dir := filepath.Join(f.r.cfg.DataDir, "rt", "checkpoints-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	results := []proto.StagedCheckpoint{{Repo: "api", Head: head, Refs: map[string]string{"HEAD": head}, Size: 4, File: "0.bundle"}}
	b, _ := json.Marshal(results)
	if err := os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *finishFixture) warnings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.reports {
		var ev proto.RunEvent
		if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == proto.EvWarning {
			msg, _ := ev.Data["message"].(string)
			out = append(out, msg)
		}
	}
	return out
}

// A FIFO the workload planted in place of a staged bundle is refused at
// once with a warning; collection neither blocks on it nor reports it.
func TestCollectCheckpointRefusesFIFO(t *testing.T) {
	f := newFinishFixture(t)
	dir := stageCheckpointDir(t, f)
	if err := syscall.Mkfifo(filepath.Join(dir, "0.bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.collectCheckpoints(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Unblock the open so the goroutine can end before the test does.
		if w, err := os.OpenFile(filepath.Join(dir, "0.bundle"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("collection blocked on a FIFO")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(strings.Join(f.warnings(), "\n"), "api: checkpoints-1/0.bundle is not a regular file") {
		if time.Now().After(deadline) {
			t.Fatalf("warnings %q", f.warnings())
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.reports {
		if fr.Type == proto.MsgCheckpointReady {
			t.Fatalf("reported a FIFO: %s", fr.Data)
		}
	}
}
