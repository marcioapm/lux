package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	c, w, log := cxStartedAt(t, rollout)
	return c, w, log, rollout
}

// cxStartedAt is a Codex adapter whose thread.path is rollout, as it is.
func cxStartedAt(t *testing.T, rollout string) (*Codex, *agentWire, *inputSink) {
	t.Helper()
	log := &inputSink{}
	c := NewCodex()
	c.compactionWait = 300 * time.Millisecond
	w := startWireSink(t, c, proto.ShimConfig{}, cxEvents{warnSink{log}})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.145.0 (Ubuntu; x86_64)"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `","path":"` + rollout + `","status":{"type":"idle"}}}}`)
	// The handshake is done (it reports idle last).
	log.wait(t, "idle")
	return c, w, log
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

// cxEvents is warnSink also logging each codex.item/* relay as
// "codex.<method> <item type>", in order with the rest.
type cxEvents struct{ warnSink }

func (s cxEvents) Event(typ string, data any) {
	if strings.HasPrefix(typ, "codex.item/") {
		raw, _ := data.(json.RawMessage)
		var p struct {
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		_ = json.Unmarshal(raw, &p)
		s.add(typ + " " + p.Item.Type)
	}
	s.warnSink.Event(typ, data)
}

// lineAfter is the line right after the first one starting with prefix, or
// "" if there is none.
func lineAfter(lines []string, prefix string) string {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			if i+1 < len(lines) {
				return lines[i+1]
			}
			return ""
		}
	}
	return ""
}

// A compaction is one lux.compacted, when its item completes (not when it
// starts), with the summary Codex wrote to the rollout, directly after the
// item/completed relay when the rollout already holds the entry. The items
// are still relayed as codex.item/*.
func TestCodexCompactionIsReported(t *testing.T) {
	_, w, log, rollout := cxCompaction(t)
	started, completed := cxItems(t)
	lines, summary := cxRolloutCompacted(t)
	appendRollout(t, rollout, lines...)
	w.send(started)
	log.wait(t, "codex.item/started contextCompaction")
	time.Sleep(200 * time.Millisecond)
	if got := log.compactions(); len(got) != 0 {
		t.Fatalf("reported on item/started: %q", got)
	}
	w.send(completed)
	// Sent again (a replay): not a second record. exit joins every read.
	w.send(completed)
	w.exit()
	want, _ := json.Marshal(proto.Compaction{SessionID: cxThread, Summary: summary})
	if got := log.compactions(); !slices.Equal(got, []string{"compacted " + string(want)}) {
		t.Fatalf("got %q", got)
	}
	if next := lineAfter(log.lines(), "codex.item/completed contextCompaction"); next != "compacted "+string(want) {
		t.Fatalf("after the item/completed relay: %q; lines %q", next, log.lines())
	}
	if log.has("warning") {
		t.Fatalf("warned: %q", log.lines())
	}
}

// Codex may write the rollout's entry just after the item completes: it is
// waited for, and reported once, after the relay.
func TestCodexCompactionSummaryWrittenLate(t *testing.T) {
	c, w, log, rollout := cxCompaction(t)
	c.compactionWait = 2 * time.Second
	started, completed := cxItems(t)
	lines, summary := cxRolloutCompacted(t)
	w.send(started)
	w.send(completed)
	log.wait(t, "codex.item/completed contextCompaction")
	time.Sleep(60 * time.Millisecond)
	appendRollout(t, rollout, lines...)
	waitCompactions(t, log, 1)
	w.exit()
	want, _ := json.Marshal(proto.Compaction{SessionID: cxThread, Summary: summary})
	l := log.lines()
	if got := log.compactions(); !slices.Equal(got, []string{"compacted " + string(want)}) {
		t.Fatalf("got %q", got)
	}
	if slices.Index(l, "compacted "+string(want)) < slices.Index(l, "codex.item/completed contextCompaction") {
		t.Fatalf("record before the relay: %q", l)
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
		// adjacent: the entry is in the rollout when the item completes.
		adjacent bool
		after    func(t *testing.T, rollout string, lines []string)
	}{
		{"never written", "no compacted entry in", false, func(*testing.T, string, []string) {}},
		{"remote", "Codex compacted remotely and exposes no summary text", true, func(t *testing.T, rollout string, lines []string) {
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
			warning := "warning codex: thread " + cxThread + " was compacted; its summary could not be read: " + tc.warning
			if next := lineAfter(log.lines(), "compacted "); !strings.HasPrefix(next, warning) {
				t.Fatalf("after the record: %q, want %q; lines %q", next, warning, log.lines())
			}
			if tc.adjacent {
				if next := lineAfter(log.lines(), "codex.item/completed contextCompaction"); next != "compacted "+string(want) {
					t.Fatalf("after the item/completed relay: %q; lines %q", next, log.lines())
				}
			}
		})
	}
}

// A rollout path that is not a regular file (a FIFO, which an open would
// block on, or a symlink, which is not followed) is not read: the Run
// goes on, and the compaction is reported without a summary.
func TestCodexRolloutNotARegularFile(t *testing.T) {
	for _, tc := range []struct {
		name, warning string
		make          func(t *testing.T, path string)
	}{
		{"fifo", "is not a regular file", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "too many levels of symbolic links", func(t *testing.T, path string) {
			lines, _ := cxRolloutCompacted(t)
			target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
			if err := os.WriteFile(target, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
			tc.make(t, rollout)
			ran := make(chan struct{})
			go func() {
				defer close(ran)
				_, w, log := cxStartedAt(t, rollout)
				started, completed := cxItems(t)
				w.send(started)
				w.send(completed)
				got := waitCompactions(t, log, 1)
				w.exit()
				want, _ := json.Marshal(proto.Compaction{SessionID: cxThread})
				if !slices.Equal(got, []string{"compacted " + string(want)}) {
					t.Errorf("got %q", got)
				}
				if next := lineAfter(log.lines(), "compacted "); !strings.Contains(next, tc.warning) {
					t.Errorf("after the record: %q, want %q", next, tc.warning)
				}
			}()
			select {
			case <-ran:
			case <-time.After(10 * time.Second):
				t.Fatal("the Run hung on the rollout")
			}
		})
	}
}

// A rollout line longer than rolloutLineMax is skipped, not held: a
// compacted entry that long is reported without its summary, and the
// entries after it keep their place.
func TestCodexRolloutLineTooLong(t *testing.T) {
	_, w, log, rollout := cxCompaction(t)
	started, completed := cxItems(t)
	lines, _ := cxRolloutCompacted(t)
	huge := `{"timestamp":"2026-10-10T15:48:19.823Z","type":"compacted","payload":{"message":"` +
		strings.Repeat("x", rolloutLineMax) + `"}}`
	// A long line of another type is skipped too.
	other := `{"timestamp":"2026-10-10T15:48:19.823Z","type":"response_item","payload":{"text":"` +
		strings.Repeat("y", rolloutLineMax) + `"}}`
	appendRollout(t, rollout, other, huge, lines[1])
	w.send(started)
	w.send(completed)
	waitCompactions(t, log, 1)
	second := strings.Replace(lines[0], "PERIWINKLE", "MARIGOLD", 1)
	appendRollout(t, rollout, second, lines[1])
	w.send(strings.ReplaceAll(started, "01a12680-3221", "01a12690-0000"))
	w.send(strings.ReplaceAll(completed, "01a12680-3221", "01a12690-0000"))
	got := waitCompactions(t, log, 2)
	w.exit()
	want, _ := json.Marshal(proto.Compaction{SessionID: cxThread})
	if len(got) != 2 || got[0] != "compacted "+string(want) || !strings.Contains(got[1], "MARIGOLD") {
		t.Fatalf("got %q", got)
	}
	if !log.has("warning codex: thread " + cxThread + " was compacted; its summary could not be read: its rollout entry is longer than") {
		t.Fatalf("no warning: %q", log.lines())
	}
}
