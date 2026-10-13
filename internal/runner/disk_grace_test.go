package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanForDiskSamples answers sampleSlow's probes: `container inspect
// --size` prints $0.size, or fails while $0.failsize exists; `volume
// inspect` fails while $0.failvolume exists. stats has no network.
const podmanForDiskSamples = `#!/bin/sh
case "$1 $2" in
"container inspect")
  [ -e "$0.failsize" ] && { echo "no such container" >&2; exit 125; }
  cat "$0.size" ;;
"volume inspect")
  [ -e "$0.failvolume" ] && { echo "no such volume" >&2; exit 125; }
  echo "$0.vol" ;;
*)
  case "$1" in
  stats) echo '[{"net_io":"0B / 0B"}]' ;;
  *) exit 1 ;;
  esac ;;
esac
`

const (
	diskLimit = int64(10 << 30)
	diskOver  = diskLimit + 1<<30
	diskGrace = 5 * time.Minute
)

// diskFixture is a running placement of run1, limited to diskLimit with
// diskGrace, reporting to a fakeLuxd; logs collects the runner's log.
type diskFixture struct {
	r    *Runner
	bin  string
	luxd *fakeLuxd
	logs *bytes.Buffer
	ctx  context.Context
}

func newDiskFixture(t *testing.T) *diskFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanForDiskSamples), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin+".vol", 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewJSONHandler(&lockedWriter{w: logs}, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded()}
	r.conn = newConn(r)
	r.conn.polling = true
	luxd := &fakeLuxd{r: r}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { luxd.poll(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &diskFixture{r: r, bin: bin, luxd: luxd, logs: logs, ctx: ctx}
}

// placement is a new running placement of run1 at epoch, its container
// ctr-1 and no volumes.
func (f *diskFixture) placement(epoch int) *placement {
	sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(diskLimit), DiskGrace: spec.SetDuration(diskGrace)}}
	p := newPlacement(f.r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: epoch, Spec: sp}, nil)
	p.state = &runState{RunID: "run1", Epoch: epoch, Container: "ctr-1"}
	p.setPhase("running")
	return p
}

// events is every disk.exceeded event luxd got so far, after a pause long
// enough for one reported in the background to arrive.
func (f *diskFixture) events(t *testing.T) []proto.RunEvent {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	f.luxd.mu.Lock()
	defer f.luxd.mu.Unlock()
	var out []proto.RunEvent
	for _, fr := range f.luxd.frames {
		var ev proto.RunEvent
		if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == proto.EvDiskExceeded {
			out = append(out, ev)
		}
	}
	return out
}

// wentOver is the "remaining" of every log record of a sample going over,
// in nanoseconds (slog's JSON encoding of a time.Duration).
func (f *diskFixture) wentOver(t *testing.T) []time.Duration {
	t.Helper()
	var out []time.Duration
	for _, l := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec struct {
			Msg       string        `json:"msg"`
			Remaining time.Duration `json:"remaining"`
		}
		if json.Unmarshal([]byte(l), &rec) == nil && strings.HasPrefix(rec.Msg, "over its disk limit; ") {
			out = append(out, rec.Remaining)
		}
	}
	return out
}

func (f *diskFixture) flag(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(f.bin+"."+name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// size sets what `container inspect --size` answers.
func (f *diskFixture) size(t *testing.T, n int64) {
	t.Helper()
	if err := os.WriteFile(f.bin+".size", []byte(strconv.FormatInt(n, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// Within its grace a Run over its disk is not reported; once over for the
// whole grace it is, once, with what it used and was held to; a later
// sample over sends nothing more.
func TestCheckDiskReportsOnceAfterTheGrace(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	t0 := time.Now()
	p.checkDisk(f.ctx, diskOver, t0)
	p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace-time.Second))
	if evs := f.events(t); len(evs) != 0 {
		t.Fatalf("within the grace: %+v, want no event", evs)
	}
	p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
	evs := f.events(t)
	if len(evs) != 1 {
		t.Fatalf("grace elapsed: %d events %+v, want one", len(evs), evs)
	}
	want := map[string]any{"usedBytes": float64(diskOver), "limitBytes": float64(diskLimit), "graceSeconds": diskGrace.Seconds(), "ceiling": false}
	if got := evs[0].Data; len(got) != len(want) || got["usedBytes"] != want["usedBytes"] || got["limitBytes"] != want["limitBytes"] ||
		got["graceSeconds"] != want["graceSeconds"] || got["ceiling"] != want["ceiling"] {
		t.Errorf("event data %v, want %v", got, want)
	}
	p.checkDisk(f.ctx, 3*diskLimit, t0.Add(diskGrace+15*time.Second))
	if evs := f.events(t); len(evs) != 1 {
		t.Errorf("after the report: %d events, want still one", len(evs))
	}
}

// A placement that is not running is never sampled against its disk, even
// at the ceiling.
func TestCheckDiskOnlyWhileRunning(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	p.setPhase("exited")
	p.checkDisk(f.ctx, 3*diskLimit, time.Now())
	if evs := f.events(t); len(evs) != 0 {
		t.Fatalf("exited at 3× its disk: %+v, want no event", evs)
	}
}

// A sample whose probe failed reads as under the limit (its sum misses the
// failed part) and must not restart the grace; a successful sample under
// the limit does.
func TestFailedDiskProbeKeepsTheGrace(t *testing.T) {
	for _, c := range []struct {
		name   string
		flag   string
		volume bool
		peak   int64 // the partial sum: what the probes that answered measured
	}{
		{"container inspect --size fails", "failsize", false, 0},
		{"a volume's mountpoint fails", "failvolume", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDiskFixture(t)
			p := f.placement(1)
			if c.volume {
				p.state.Volumes = []volumeRef{{Name: "data", Volume: "lux-run1-data", Kind: "state"}}
			}
			f.size(t, 1)
			f.flag(t, c.flag)
			t0 := time.Now()
			p.checkDisk(f.ctx, diskOver, t0)
			p.sampleSlow(f.ctx)
			p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
			if evs := f.events(t); len(evs) != 1 {
				t.Fatalf("over at the grace's end after a failed probe: %d events, want one", len(evs))
			}
			p.mu.Lock()
			peak := p.peakDisk
			p.mu.Unlock()
			if peak != c.peak {
				t.Errorf("peak disk %d after the partial sample, want %d", peak, c.peak)
			}
		})
	}
	// The same steps with every probe answering: the sample under the limit
	// restarts the grace, so the sample at its end is the first over again.
	t.Run("control: all probes succeed", func(t *testing.T) {
		f := newDiskFixture(t)
		p := f.placement(1)
		f.size(t, 1)
		t0 := time.Now()
		p.checkDisk(f.ctx, diskOver, t0)
		p.sampleSlow(f.ctx)
		p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
		if evs := f.events(t); len(evs) != 0 {
			t.Fatalf("%d events, want none: the sample under the limit restarts the grace", len(evs))
		}
	})
}

// A new placement of the Run (a resume) starts its grace from zero: its
// first sample over has the whole grace left, even though the last
// placement was reported.
func TestNewPlacementGetsAFreshGrace(t *testing.T) {
	f := newDiskFixture(t)
	t0 := time.Now()
	p1 := f.placement(1)
	p1.checkDisk(f.ctx, diskOver, t0)
	p1.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
	if evs := f.events(t); len(evs) != 1 {
		t.Fatalf("first placement: %d events, want one", len(evs))
	}
	p2 := f.placement(2)
	t1 := t0.Add(diskGrace + time.Minute)
	p2.checkDisk(f.ctx, diskOver, t1)
	p2.checkDisk(f.ctx, diskOver, t1.Add(diskGrace-time.Second))
	if evs := f.events(t); len(evs) != 1 {
		t.Fatalf("second placement within its grace: %d events, want still one", len(evs))
	}
	if got := f.wentOver(t); len(got) != 2 || got[1] != diskGrace {
		t.Errorf("went-over logs' remaining %v, want the second %s", got, diskGrace)
	}
	p2.checkDisk(f.ctx, diskOver, t1.Add(diskGrace))
	if evs := f.events(t); len(evs) != 2 {
		t.Fatalf("second placement past its grace: %d events, want two", len(evs))
	}
}
