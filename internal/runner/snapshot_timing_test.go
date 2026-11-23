package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/marcioapm/lux/internal/proto"
)

// Exercise finish -> snapshot -> VolumeExport -> the real blob writer. Only
// the fixture's podman export is replaced; reporting and compression stay real.
func TestSnapshotTiming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		outcome string
	}{
		{"payload", strings.Repeat("snapshot payload\x00\xff\n", 8192), "success"},
		{"empty", "", "success"},
		{"partial export failure", strings.Repeat("partial export\n", 8192), "failed"},
		{"cancelled export", strings.Repeat("before cancellation\n", 8192), "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinishFixture(t)
			// Preserve the fixture's other podman commands (sampleSlow, remove,
			// etc.), changing only export to supply deterministic bytes.
			script, err := os.ReadFile(f.bin)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.bin+".payload", tc.payload)
			export := "  cat \"$0.payload\"\n"
			switch tc.outcome {
			case "failed":
				export += "  echo 'partial export failed' >&2; exit 1 ;;"
			case "cancelled":
				// exec leaves no child holding the stdout pipe after cancellation.
				export += "  touch \"$0.exporting\"\n  exec sleep 60 ;;"
			default:
				export += "  exit 0 ;;"
			}
			start := strings.Index(string(script), "  [ -e \"$0.failexport\" ]")
			end := strings.Index(string(script), "\n\"rm -f\")")
			if start < 0 || end <= start {
				t.Fatal("finish fixture's export command changed")
			}
			if err := os.WriteFile(f.bin, []byte(string(script[:start])+export+string(script[end:])), 0o755); err != nil {
				t.Fatal(err)
			}

			f.exitedAsSupervised(context.Background())
			if tc.outcome == "cancelled" {
				f.waitFile(t, f.bin+".exporting")
				f.p.mu.Lock()
				cancel := f.p.cancelExport
				f.p.mu.Unlock()
				if cancel == nil {
					t.Fatal("finish did not install its export cancellation")
				}
				cancel()
			}
			// Event sends are asynchronous: finish returning does not mean its
			// terminal events have reached the fake luxd yet.
			mustEnd(t, f.p, "finish did not return")
			terminal := "completed"
			if tc.outcome != "success" {
				terminal = "failed"
			}
			want := []string{"snapshot.local.started", "snapshot.local." + terminal,
				"snapshot.volume.started", "snapshot.volume." + terminal}
			var events map[string][]proto.RunEvent
			waitFor(t, "snapshot timing events did not arrive", func() bool {
				events = snapshotTimingEvents(t, f)
				for _, typ := range want {
					if len(events[typ]) == 0 {
						return false
					}
				}
				return true
			})
			if len(events) != len(want) {
				t.Fatalf("event types %v, want only %v (no successful terminal on failure)", events, want)
			}
			var snapshotID string
			for _, typ := range want {
				if len(events[typ]) != 1 {
					t.Fatalf("%s: got %d events, want exactly one", typ, len(events[typ]))
				}
				ev := events[typ][0]
				id, ok := ev.Data["snapshot_id"].(string)
				if !ok || id == "" {
					t.Fatalf("%s: missing snapshot_id: %+v", typ, ev)
				}
				if snapshotID == "" {
					snapshotID = id
				}
				if id != snapshotID {
					t.Errorf("%s: snapshot_id %q, want %q", typ, id, snapshotID)
				}
				if strings.HasPrefix(typ, "snapshot.volume.") {
					for key, expected := range map[string]string{"volume": "data", "codec": "zstd", "level": "default"} {
						if ev.Data[key] != expected {
							t.Errorf("%s: %s = %v, want %q", typ, key, ev.Data[key], expected)
						}
					}
					if n := snapshotTimingNumber(t, ev, "concurrency"); n != 1 {
						t.Errorf("%s: concurrency %v, want 1", typ, n)
					}
				}
				if !strings.HasSuffix(typ, ".started") {
					if ev.Data["outcome"] != tc.outcome {
						t.Errorf("%s: outcome %v, want %s", typ, ev.Data["outcome"], tc.outcome)
					}
					snapshotTimingNumber(t, ev, "elapsed_ms")
				}
			}
			volume := events["snapshot.volume."+terminal][0]
			for _, key := range []string{"stream_ms", "finalize_ms", "sync_ms"} {
				snapshotTimingNumber(t, volume, key)
			}
			if n := snapshotTimingNumber(t, volume, "raw_bytes"); n != float64(len(tc.payload)) {
				t.Errorf("raw_bytes %v, want exactly %d (including partial exports)", n, len(tc.payload))
			}
			// Mirror the production encoder configuration, not a hard-coded
			// size tied to a particular zstd library version.
			var compressed bytes.Buffer
			zw, err := zstd.NewWriter(&compressed, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(zw, strings.NewReader(tc.payload)); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if n := snapshotTimingNumber(t, volume, "compressed_bytes"); n != float64(compressed.Len()) {
				t.Errorf("compressed_bytes %v, want %d", n, compressed.Len())
			}
			if tc.outcome == "success" {
				sd := f.snapshotDone(t)
				if sd.Error != "" || sd.Manifest.SnapshotID != snapshotID || len(sd.Manifest.Volumes) != 1 {
					t.Fatalf("unexpected snapshot.done: %+v", sd)
				}
				v := sd.Manifest.Volumes[0]
				blob, err := os.ReadFile(f.r.blobPath(v.BlobID))
				if err != nil {
					t.Fatal(err)
				}
				if int64(len(blob)) != v.Size || len(blob) != compressed.Len() {
					t.Errorf("blob size %d, manifest %d, measured %d", len(blob), v.Size, compressed.Len())
				}
				zr, err := zstd.NewReader(nil)
				if err != nil {
					t.Fatal(err)
				}
				defer zr.Close()
				decoded, err := zr.DecodeAll(blob, nil)
				if err != nil || !bytes.Equal(decoded, []byte(tc.payload)) {
					t.Fatalf("zstd roundtrip: decoded %d bytes, want %d; error %v", len(decoded), len(tc.payload), err)
				}
			} else {
				if len(f.r.snapshotRecords()) != 0 {
					t.Error("failed export left a successful snapshot record")
				}
				if tc.outcome == "failed" && f.snapshotDone(t).Error == "" {
					t.Error("failed export reported snapshot.done without an error")
				}
			}
		})
	}
}

func snapshotTimingEvents(t *testing.T, f *finishFixture) map[string][]proto.RunEvent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]proto.RunEvent)
	for _, fr := range f.reports {
		if fr.Type != proto.MsgRunEvent {
			continue
		}
		var ev proto.RunEvent
		if err := json.Unmarshal(fr.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ev.Type, "snapshot.local.") && !strings.HasPrefix(ev.Type, "snapshot.volume.") {
			continue
		}
		if fr.RunID != "run1" || fr.Epoch != 1 {
			t.Errorf("%s envelope: run %q epoch %d, want run1 epoch 1", ev.Type, fr.RunID, fr.Epoch)
		}
		out[ev.Type] = append(out[ev.Type], ev)
	}
	return out
}

func snapshotTimingNumber(t *testing.T, ev proto.RunEvent, key string) float64 {
	t.Helper()
	n, ok := ev.Data[key].(float64) // RunEvent has crossed the JSON wire.
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		t.Fatalf("%s: %s = %v, want a finite nonnegative JSON number", ev.Type, key, ev.Data[key])
	}
	return n
}
