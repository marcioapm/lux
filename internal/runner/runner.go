// Package runner is lux-runner: the agent on every host. It dials luxd,
// takes placements, runs them in hardened Podman containers with lux-shim
// as PID 1, relays their output, snapshots their state volumes whenever a
// container exits, and uploads what luxd needs to resume them elsewhere.
//
// Podman is daemonless: every container has its own conmon, so restarting
// the runner does not touch running containers. On start the runner
// re-adopts them by label.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/version"
)

type Config struct {
	URL     string
	Token   string
	Name    string
	DataDir string
	Shim    string
	Labels  map[string]string
	MaxRuns int
	CPUs    float64
	Memory  int64
	// ProviderID is the cloud instance id, for provisioned hosts.
	ProviderID string
	// ForcePoll uses the HTTP polling transport even if WebSocket works.
	ForcePoll bool
	// HostTTL is how long local snapshot copies are kept after upload.
	HostTTL time.Duration
}

type Runner struct {
	cfg    Config
	log    *slog.Logger
	pm     *podman.Podman
	conn   *conn
	api    *api
	shimV  string
	leaseS float64

	mu         sync.Mutex
	placements map[string]*placement // by run id
	subs       map[string]context.CancelFunc
	uploads    *uploader
	control    *serialQueues
	git        *gitws.Manager
	mounts     sync.Map // volume name → mountpoint
}

// mountpoint is where a volume's data is on this host. It never changes for
// a volume's life, so it is looked up once (each lookup forks podman).
func (r *Runner) mountpoint(ctx context.Context, volume string) (string, error) {
	if mp, ok := r.mounts.Load(volume); ok {
		return mp.(string), nil
	}
	mp, err := r.pm.VolumeMountpoint(ctx, volume)
	if err != nil {
		return "", err
	}
	r.mounts.Store(volume, mp)
	return mp, nil
}

func (r *Runner) forgetMountpoint(volume string) { r.mounts.Delete(volume) }

func New(cfg Config, log *slog.Logger) (*Runner, error) {
	if cfg.Name == "" {
		h, _ := os.Hostname()
		cfg.Name = h
	}
	if cfg.MaxRuns == 0 {
		cfg.MaxRuns = 16
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = float64(runtime.NumCPU())
	}
	if cfg.Memory == 0 {
		cfg.Memory = memTotal()
	}
	if cfg.HostTTL == 0 {
		cfg.HostTTL = 24 * time.Hour
	}
	for _, d := range []string{"runs", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(cfg.Shim); err != nil {
		return nil, fmt.Errorf("shim binary: %w", err)
	}
	r := &Runner{
		cfg:        cfg,
		log:        log,
		pm:         podman.New(),
		placements: map[string]*placement{},
		subs:       map[string]context.CancelFunc{},
		control:    newSerialQueues(),
		git:        gitws.New(cfg.DataDir),
		leaseS:     30,
	}
	r.api = newAPI(cfg.URL, cfg.Token, cfg.Name)
	r.conn = newConn(r)
	r.uploads = newUploader(r)
	r.shimV = version.Version
	return r, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if _, err := r.pm.Version(ctx); err != nil {
		return fmt.Errorf("podman is not usable: %w", err)
	}
	r.readopt(ctx)
	go r.uploads.loop(ctx)
	go r.heartbeatLoop(ctx)
	go r.usageLoop(ctx)
	go r.gcLoop(ctx)
	r.conn.loop(ctx)
	return nil
}

func (r *Runner) hello(ctx context.Context) proto.Hello {
	pv, _ := r.pm.Version(ctx)
	imgs, _ := r.pm.Images(ctx)
	h := proto.Hello{
		Name:            r.cfg.Name,
		ProviderID:      r.cfg.ProviderID,
		ProtocolVersion: proto.Version,
		RunnerVersion:   version.Version,
		ShimVersion:     r.shimV,
		PodmanVersion:   pv,
		Arch:            runtime.GOARCH,
		Labels:          r.cfg.Labels,
		Capacity:        proto.Capacity{CPUs: r.cfg.CPUs, Memory: r.cfg.Memory, Runs: r.cfg.MaxRuns},
		Images:          imgs,
		GitMirrors:      r.git.Mirrors(),
		LocalSnapshots:  r.localSnapshots(),
	}
	r.mu.Lock()
	for _, p := range r.placements {
		if st := p.liveState(); st != "" {
			h.Live = append(h.Live, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st})
		}
	}
	r.mu.Unlock()
	return h
}

// onWelcome reconciles with what luxd thinks runs here: anything else is
// stale (its Run moved on while this host was away) and is stopped.
func (r *Runner) onWelcome(ctx context.Context, w proto.Welcome) {
	if w.LeaseSeconds > 0 {
		r.leaseS = w.LeaseSeconds
	}
	want := map[string]int{}
	for _, l := range w.Live {
		want[l.RunID] = l.Epoch
	}
	r.mu.Lock()
	var stale []*placement
	for _, p := range r.placements {
		if p.liveState() == "" {
			continue
		}
		if e, ok := want[p.runID]; !ok || e != p.epoch {
			stale = append(stale, p)
		}
	}
	r.mu.Unlock()
	for _, p := range stale {
		r.log.Warn("stopping stale placement", "run", p.runID, "epoch", p.epoch)
		p.markStale()
		go p.kill(context.WithoutCancel(ctx))
	}
}

// handleControl handles one durable message from luxd. Idempotent.
func (r *Runner) handleControl(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgAssign:
		var a proto.Assign
		if err := json.Unmarshal(f.Data, &a); err != nil {
			r.log.Error("bad assign", "err", err)
			return
		}
		r.assign(ctx, a)
	case proto.MsgStop, proto.MsgCancel:
		var s proto.StopRequest
		_ = json.Unmarshal(f.Data, &s)
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.requestStop(ctx, s.Reason)
		}
	case proto.MsgInput:
		var in proto.Input
		_ = json.Unmarshal(f.Data, &in)
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.input(ctx, in)
		}
	case proto.MsgInterrupt:
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.interrupt(ctx)
		}
	case proto.MsgPush:
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			var req struct {
				RequestID string `json:"requestId"`
				Message   string `json:"message"`
			}
			_ = json.Unmarshal(f.Data, &req)
			p.push(ctx, req.RequestID, req.Message)
		}
	case proto.MsgSnapshotDiscard:
		var d struct {
			RunID       string `json:"runId"`
			BeforeEpoch int    `json:"beforeEpoch"`
		}
		_ = json.Unmarshal(f.Data, &d)
		r.discard(ctx, d.RunID, d.BeforeEpoch)
	case proto.MsgDrain:
		// luxd stops this host's Runs itself; nothing to do locally.
	default:
		r.log.Warn("unknown control message", "type", f.Type)
	}
}

func (r *Runner) placement(runID string, epoch int) *placement {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.placements[runID]
	if p == nil || (epoch != 0 && p.epoch != epoch) {
		return nil
	}
	return p
}

func (r *Runner) assign(ctx context.Context, a proto.Assign) {
	r.mu.Lock()
	old := r.placements[a.RunID]
	if old != nil && old.epoch == a.Epoch {
		r.mu.Unlock()
		return // redelivered
	}
	if old != nil && old.epoch > a.Epoch {
		r.mu.Unlock()
		r.log.Warn("ignoring assign for an older epoch", "run", a.RunID, "epoch", a.Epoch, "have", old.epoch)
		return
	}
	p := newPlacement(r, a)
	r.placements[a.RunID] = p
	r.mu.Unlock()
	if old != nil && old.liveState() != "" {
		// The same Run again with a newer epoch while the old one still
		// runs here: luxd gave up on the old one.
		old.markStale()
		old.kill(ctx)
		old.waitDone(30 * time.Second)
	}
	go p.run(context.WithoutCancel(ctx))
}

// handleLive handles non-durable frames: output subscriptions, streams.
func (r *Runner) handleLive(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgOutputSubscribe:
		var s proto.OutputSubscribe
		_ = json.Unmarshal(f.Data, &s)
		sctx, cancel := context.WithCancel(ctx)
		r.mu.Lock()
		r.subs[s.SubID] = cancel
		r.mu.Unlock()
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.subs, s.SubID)
			r.mu.Unlock()
		}()
		err := r.streamOutput(sctx, f.RunID, f.Epoch, s)
		end := proto.OutputEnd{SubID: s.SubID}
		if err != nil && sctx.Err() == nil {
			end.Error = err.Error()
		}
		if sctx.Err() == nil {
			_ = r.conn.Send(ctx, proto.Frame{Type: proto.MsgOutputEnd, RunID: f.RunID, Epoch: f.Epoch, Data: proto.Marshal(end)})
		}
	case proto.MsgOutputCancel:
		var s proto.OutputSubscribe
		_ = json.Unmarshal(f.Data, &s)
		r.mu.Lock()
		if c := r.subs[s.SubID]; c != nil {
			c()
		}
		r.mu.Unlock()
	case proto.MsgStreamOpen, proto.MsgStreamData, proto.MsgStreamClose:
		r.handleStream(ctx, f)
	}
}

func (r *Runner) heartbeatLoop(ctx context.Context) {
	for {
		interval := time.Duration(r.leaseS / 3 * float64(time.Second))
		if interval < time.Second {
			interval = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		hb := proto.Heartbeat{LocalSnapshots: r.localSnapshots()}
		for _, p := range r.livePlacements() {
			if st := p.liveState(); st != "" {
				hb.Leases = append(hb.Leases, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st, Usage: p.usage()})
			}
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = r.conn.Report(hctx, proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(hb)})
		cancel()
	}
}

func (r *Runner) livePlacements() []*placement {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := make([]*placement, 0, len(r.placements))
	for _, p := range r.placements {
		if p.liveState() != "" {
			ps = append(ps, p)
		}
	}
	return ps
}

// usageLoop samples disk and network use of live placements, in parallel,
// on a slower schedule than heartbeats: it walks volumes and forks podman.
func (r *Runner) usageLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for _, p := range r.livePlacements() {
			wg.Go(func() { p.sampleSlow(ctx) })
		}
		wg.Wait()
	}
}

// localSnapshots lists Runs whose state volumes are on this host.
func (r *Runner) localSnapshots() []proto.LocalSnapshot {
	var out []proto.LocalSnapshot
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
	for _, e := range entries {
		st, err := readRunState(r.runDir(e.Name()))
		if err != nil || st.VolumesSnapshot == "" {
			continue
		}
		out = append(out, proto.LocalSnapshot{RunID: e.Name(), Epoch: st.VolumesEpoch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

func (r *Runner) runDir(runID string) string { return filepath.Join(r.cfg.DataDir, "runs", runID) }

func memTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}
