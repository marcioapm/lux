package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// cxCompaction is a Codex adapter whose thread's rollout (thread/start's
// thread.path) is a file in a temporary directory holding prior lines.
func cxCompaction(t *testing.T, prior ...string) (*Codex, *agentWire, *inputSink, string) {
	t.Helper()
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendRollout(t, rollout, prior...)
	log := &inputSink{}
	c := NewCodex()
	c.compactionWait = 300 * time.Millisecond
	w := startWireSink(t, c, proto.ShimConfig{}, warnSink{log})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.145.0 (Ubuntu; x86_64)"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `","path":"` + rollout + `","status":{"type":"idle"}}}}`)
	// The handshake is done (it reports idle last).
	log.wait(t, "idle")
	return c, w, log, rollout
}

// cxItems are codex 0.145.0's contextCompaction item/started and
// item/completed for a thread/compact/start
// (testdata/codex-0.145.0-compaction-items.jsonl), on cxThread.
func cxItems(t *testing.T) (started, completed string) {
	t.Helper()
	b, err := os.ReadFile("testdata/codex-0.145.0-compaction-items.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	l := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(b), "01a12680-2d6c-79d1-9c30-ce71d2847d5e", cxThread)), "\n")
	return l[0], l[1]
}

// appendRollout appends lines to the rollout, as Codex writes it.
func appendRollout(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// cxRolloutCompacted is the rollout's compacted entry and context_compacted
// event from that compaction (testdata/codex-0.145.0-rollout-compacted.jsonl).
func cxRolloutCompacted(t *testing.T) ([]string, string) {
	t.Helper()
	b, err := os.ReadFile("testdata/codex-0.145.0-rollout-compacted.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e struct {
		Payload struct {
			Message string `json:"message"`
		} `json:"payload"`
	}
	_ = json.Unmarshal([]byte(lines[0]), &e)
	return lines, e.Payload.Message
}

// A compaction is one lux.compacted, when its item completes (not when it
// starts), with the summary Codex wrote to the rollout. The items are
// still relayed as codex.item/*.
func TestCodexCompactionIsReported(t *testing.T) {
	_, w, log, rollout := cxCompaction(t)
	started, completed := cxItems(t)
	lines, summary := cxRolloutCompacted(t)
	w.send(started)
	appendRollout(t, rollout, lines...)
	w.send(completed)
	// Sent again (a replay): not a second record.
	w.send(completed)
	got := waitCompactions(t, log, 1)
	time.Sleep(50 * time.Millisecond)
	w.exit()
	want, _ := json.Marshal(proto.Compaction{SessionID: cxThread, Summary: summary})
	if got = log.compactions(); !slices.Equal(got, []string{"compacted " + string(want)}) {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(summary, "PERIWINKLE") || log.has("warning") {
		t.Fatalf("summary %q, lines %q", summary, log.lines())
	}
}

// Codex may write the rollout's entry just after the item completes: it is
// waited for.
func TestCodexCompactionSummaryWrittenLate(t *testing.T) {
	_, w, log, rollout := cxCompaction(t)
	started, completed := cxItems(t)
	lines, summary := cxRolloutCompacted(t)
	w.send(started)
	w.send(completed)
	time.Sleep(60 * time.Millisecond)
	appendRollout(t, rollout, lines...)
	got := waitCompactions(t, log, 1)
	w.exit()
	want, _ := json.Marshal(proto.Compaction{SessionID: cxThread, Summary: summary})
	if !slices.Equal(got, []string{"compacted " + string(want)}) {
		t.Fatalf("got %q", got)
	}
}

// Two compactions in a Run: each record has its own entry's summary.
func TestCodexTwoCompactions(t *testing.T) {
	_, w, log, rollout := cxCompaction(t)
	started, completed := cxItems(t)
	lines, summary := cxRolloutCompacted(t)
	w.send(started)
	appendRollout(t, rollout, lines...)
	w.send(completed)
	waitCompactions(t, log, 1)
	second := strings.Replace(lines[0], "PERIWINKLE", "MARIGOLD", 1)
	w.send(strings.ReplaceAll(started, "01a12680-3221", "01a12690-0000"))
	appendRollout(t, rollout, second, lines[1])
	w.send(strings.ReplaceAll(completed, "01a12680-3221", "01a12690-0000"))
	got := waitCompactions(t, log, 2)
	w.exit()
	if len(got) != 2 || !strings.Contains(got[0], "PERIWINKLE") || !strings.Contains(got[1], "MARIGOLD") ||
		strings.Count(summary, "PERIWINKLE") == 0 {
		t.Fatalf("got %q", got)
	}
}

// A compaction whose summary is not in the rollout (none written in time,
// or an empty message: remote compaction) is reported without one, with a
// warning saying why. A compacted entry the rollout held when the thread
// started (a resumed thread's) is not taken for it.
func TestCodexCompactionWithoutSummary(t *testing.T) {
	for _, tc := range []struct {
		name, warning string
		after         func(t *testing.T, rollout string, lines []string)
	}{
		{"never written", "no compacted entry in", func(*testing.T, string, []string) {}},
		{"remote", "Codex compacted remotely and exposes no summary text", func(t *testing.T, rollout string, lines []string) {
			var e map[string]any
			_ = json.Unmarshal([]byte(lines[0]), &e)
			e["payload"].(map[string]any)["message"] = ""
			b, _ := json.Marshal(e)
			appendRollout(t, rollout, string(b), lines[1])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, _ := cxRolloutCompacted(t)
			_, w, log, rollout := cxCompaction(t, lines...)
			started, completed := cxItems(t)
			w.send(started)
			tc.after(t, rollout, lines)
			w.send(completed)
			got := waitCompactions(t, log, 1)
			w.exit()
			want, _ := json.Marshal(proto.Compaction{SessionID: cxThread})
			if !slices.Equal(got, []string{"compacted " + string(want)}) {
				t.Fatalf("got %q", got)
			}
			if !log.has("warning codex: thread " + cxThread + " was compacted; its summary could not be read: " + tc.warning) {
				t.Fatalf("no warning: %q", log.lines())
			}
		})
	}
}
