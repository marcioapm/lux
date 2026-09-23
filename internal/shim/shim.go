// Package shim is lux-shim: PID 1 in every container.
//
// It is the only process inside the container that knows about lux. It
// runs the init script and the workload, captures output (redacted) into
// the placement's output file, runs the adapter that speaks the workload's
// protocol, accepts input and stop requests from the runner over a Unix
// socket, reaps zombies, and reports how the workload exited. It never
// talks to the network.
package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/adapter"
	"github.com/marcioapm/lux/internal/proto"
)

type Shim struct {
	cfg     proto.ShimConfig
	out     *Output
	red     *Redactor
	adapter adapter.Adapter
	user    *userInfo

	mu        sync.Mutex
	started   bool
	startCh   chan proto.ShimMsg
	delivered map[string]bool
	stopping  bool
	stopWhy   string
	proc      *adapter.Process
	workPid   int
	exitCh    chan syscall.WaitStatus
	initPid   int
	initCh    chan syscall.WaitStatus
	pending   []proto.Input
}

// Main is the shim's entry point. It returns the process exit code.
func Main() int {
	cfgBytes, err := os.ReadFile(proto.ShimConfigFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: no config:", err)
		return 125
	}
	var cfg proto.ShimConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: bad config:", err)
		return 125
	}
	s := &Shim{
		cfg:       cfg,
		red:       NewRedactor(nil),
		startCh:   make(chan proto.ShimMsg, 1),
		delivered: map[string]bool{},
		exitCh:    make(chan syscall.WaitStatus, 1),
		initCh:    make(chan syscall.WaitStatus, 1),
	}
	return s.run()
}

func (s *Shim) run() int {
	out, err := OpenOutput(filepath.Join(proto.ShimRunDir, proto.OutputFile(s.cfg.Epoch)), s.red)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: output:", err)
		return 125
	}
	s.out = out

	// PID 1 must reap: orphans reparent to us.
	go s.reap()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigs {
			s.stop("signal")
		}
	}()

	ln, err := s.listen()
	if err != nil {
		return s.fail("start-failed", "socket: "+err.Error())
	}
	go s.serve(ln)

	// Wait for the runner's start (it carries the secret values).
	var start proto.ShimMsg
	select {
	case start = <-s.startCh:
	case <-time.After(10 * time.Minute):
		return s.fail("start-failed", "no start from the runner within 10 minutes")
	}
	if s.isStopping() {
		return s.finish(proto.ExitInfo{ExitCode: 0, Reason: "stopped", Message: "stopped before start"})
	}
	s.red.Set(start.Secrets)

	s.user, err = lookupUser(s.cfg.User)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	env := s.environment(start.Secrets)
	if err := s.writeSecretFiles(start.Secrets); err != nil {
		return s.fail("start-failed", "secrets: "+err.Error())
	}
	s.prepareVolumes()

	if strings.TrimSpace(s.cfg.Init) != "" {
		code, err := s.runInit(env)
		if err != nil || code != 0 {
			msg := fmt.Sprintf("init script exited with %d", code)
			if err != nil {
				msg = "init script: " + err.Error()
			}
			return s.finish(proto.ExitInfo{ExitCode: max(code, 1), Reason: "init-failed", Message: msg})
		}
	}
	if s.isStopping() {
		return s.finish(proto.ExitInfo{ExitCode: 0, Reason: "stopped", Message: "stopped during init"})
	}

	ad, err := adapter.New(s.cfg.Adapter)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	s.adapter = ad
	argv, err := ad.Command(s.cfg)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	proc, err := s.startWorkload(argv, env)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	s.out.Event(proto.EvWorkload, map[string]any{"phase": "start", "pid": proc.Cmd.Process.Pid, "command": argv})

	adDone := make(chan struct{})
	go func() {
		defer close(adDone)
		if err := ad.Run(context.Background(), proc, s.cfg, &sink{s}); err != nil {
			s.out.Event(proto.EvWarning, map[string]any{"message": "adapter: " + err.Error()})
		}
	}()
	// Inputs that arrived before the workload started, then the resume
	// input from the start message.
	s.mu.Lock()
	queued := s.pending
	s.pending = nil
	s.mu.Unlock()
	if start.Input != nil {
		queued = append([]proto.Input{*start.Input}, queued...)
	}
	for _, in := range queued {
		s.deliver(in)
	}

	ws := <-s.exitCh
	// The workload's group may have children still holding the pipes.
	_ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-adDone:
	case <-time.After(5 * time.Second):
	}
	info := proto.ExitInfo{ExitCode: ws.ExitStatus(), Reason: "exited"}
	if ws.Signaled() {
		info.Signal = ws.Signal().String()
		info.ExitCode = 128 + int(ws.Signal())
	}
	if s.isStopping() {
		info.Reason = "stopped"
		info.Message = s.stopWhy
	}
	return s.finish(info)
}

func (s *Shim) fail(reason, msg string) int {
	fmt.Fprintln(os.Stderr, "lux-shim:", msg)
	s.out.Event(proto.EvWarning, map[string]any{"message": msg})
	return s.finish(proto.ExitInfo{ExitCode: 125, Reason: reason, Message: msg})
}

func (s *Shim) finish(info proto.ExitInfo) int {
	info.LastSeq = s.out.Seq()
	s.out.Close()
	b, _ := json.Marshal(info)
	path := filepath.Join(proto.ShimRunDir, proto.ExitFile(s.cfg.Epoch))
	_ = os.WriteFile(path+".tmp", b, 0o644)
	_ = os.Rename(path+".tmp", path)
	os.Remove(proto.ShimSocket)
	return info.ExitCode
}

func (s *Shim) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// reap collects every child: the workload and init script, whose status
// is handed on, and orphans, which are just reaped.
func (s *Shim) reap() {
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	for range sigchld {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			s.mu.Lock()
			work, init := s.workPid, s.initPid
			s.mu.Unlock()
			switch pid {
			case work:
				s.exitCh <- ws
			case init:
				s.initCh <- ws
			}
		}
	}
}

func (s *Shim) listen() (net.Listener, error) {
	os.Remove(proto.ShimSocket)
	ln, err := net.Listen("unix", proto.ShimSocket)
	if err != nil {
		return nil, err
	}
	// Only root in the container (the shim, and the runner through the
	// idmapped mount) may connect; the workload runs as another user.
	_ = os.Chmod(proto.ShimSocket, 0o600)
	return ln, nil
}

// serve handles runner connections. A runner that restarts reconnects;
// several connections may come and go over the container's life.
func (s *Shim) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *Shim) handleConn(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var m proto.ShimMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			enc.Encode(proto.ShimMsg{Type: "error", Error: "bad message"})
			continue
		}
		reply := proto.ShimMsg{Type: m.Type, OK: true}
		switch m.Type {
		case proto.ShimPing:
		case proto.ShimStart:
			s.mu.Lock()
			first := !s.started
			s.started = true
			s.mu.Unlock()
			if first {
				s.startCh <- m
			}
		case proto.ShimInput:
			if m.Input != nil {
				s.deliver(*m.Input)
			}
		case proto.ShimInterrupt:
			s.mu.Lock()
			ad := s.adapter
			s.mu.Unlock()
			if ad != nil {
				if err := ad.Interrupt(); err != nil {
					reply.OK, reply.Error = false, err.Error()
				}
			}
		case proto.ShimStop:
			s.stop(m.Reason)
		default:
			reply.OK, reply.Error = false, "unknown message "+m.Type
		}
		if err := enc.Encode(reply); err != nil {
			return
		}
	}
}

// deliver hands input to the adapter once per request id: the runner
// redelivers after reconnecting.
func (s *Shim) deliver(in proto.Input) {
	s.mu.Lock()
	if in.RequestID != "" && s.delivered[in.RequestID] {
		s.mu.Unlock()
		return
	}
	ad := s.adapter
	if ad == nil || s.proc == nil {
		s.pending = append(s.pending, in)
		s.mu.Unlock()
		return
	}
	if in.RequestID != "" {
		s.delivered[in.RequestID] = true
	}
	s.mu.Unlock()
	ad.Deliver(in)
}

// stop winds the workload down: the adapter's graceful stop, then SIGKILL
// to its whole process group after the grace period.
func (s *Shim) stop(reason string) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.stopWhy = reason
	ad, proc, initPid := s.adapter, s.proc, s.initPid
	s.mu.Unlock()
	s.out.Event(proto.EvStop, map[string]any{"reason": reason})
	if initPid > 0 {
		_ = syscall.Kill(-initPid, syscall.SIGTERM)
	}
	if ad != nil && proc != nil {
		if err := ad.Stop(); err != nil {
			s.out.Event(proto.EvWarning, map[string]any{"message": "graceful stop: " + err.Error()})
		}
		grace := time.Duration(s.cfg.GraceSec * float64(time.Second))
		if grace <= 0 {
			grace = 30 * time.Second
		}
		time.AfterFunc(grace, func() {
			_ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL)
		})
	}
	// Not started yet: unblock the wait for start.
	select {
	case s.startCh <- proto.ShimMsg{Type: proto.ShimStart}:
	default:
	}
}

func (s *Shim) environment(secrets map[string]string) []string {
	env := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	// Keep what the image set (PATH, LANG, …), minus the shim's own.
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	env["HOME"] = s.user.home
	env["USER"] = s.user.name
	env["LUX_RUN_ID"] = s.cfg.RunID
	env["LUX_EPOCH"] = strconv.Itoa(s.cfg.Epoch)
	if s.cfg.ArtifactsDir != "" {
		env["LUX_ARTIFACTS"] = s.cfg.ArtifactsDir
	}
	for k, v := range s.cfg.Env {
		env[k] = v
	}
	for _, sec := range s.cfg.Secrets {
		if sec.As == "env" && !sec.RunnerOnly {
			env[sec.Name] = secrets[sec.Name]
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// writeSecretFiles puts file secrets on the tmpfs at /.lux/secrets and
// links them into place. The link can live on a state volume; the value
// never does, so it is never in a snapshot.
func (s *Shim) writeSecretFiles(values map[string]string) error {
	for _, sec := range s.cfg.Secrets {
		if sec.As != "file" || sec.RunnerOnly {
			continue
		}
		src := filepath.Join(proto.ShimSecretsDir, sec.Name)
		if err := os.WriteFile(src, []byte(values[sec.Name]), 0o400); err != nil {
			return err
		}
		_ = os.Chown(src, s.user.uid, s.user.gid)
		if err := os.MkdirAll(filepath.Dir(sec.Path), 0o755); err != nil {
			return err
		}
		os.Remove(sec.Path)
		if err := os.Symlink(src, sec.Path); err != nil {
			return err
		}
		_ = os.Lchown(sec.Path, s.user.uid, s.user.gid)
	}
	if s.user.uid != 0 {
		_ = os.Chown(proto.ShimSecretsDir, s.user.uid, s.user.gid)
	}
	return nil
}

// prepareVolumes hands a fresh (root-owned) volume's root to the workload
// user, so a non-root workload can write to it.
func (s *Shim) prepareVolumes() {
	if s.user.uid == 0 {
		return
	}
	for _, p := range s.cfg.VolumePaths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == 0 {
			_ = os.Chown(p, s.user.uid, s.user.gid)
		}
	}
	if s.cfg.ArtifactsDir != "" {
		_ = os.MkdirAll(s.cfg.ArtifactsDir, 0o755)
		_ = os.Chown(s.cfg.ArtifactsDir, s.user.uid, s.user.gid)
	}
}

func (s *Shim) command(argv []string, env []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = s.cfg.Workdir
	if cmd.Dir == "" {
		cmd.Dir = s.user.home
	}
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/"
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if s.user.uid != 0 || s.user.gid != 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.user.uid), Gid: uint32(s.user.gid), Groups: s.user.groups}
	}
	// Resolve argv[0] with the workload's PATH, not the shim's.
	if !strings.Contains(argv[0], "/") {
		for _, kv := range env {
			if p, ok := strings.CutPrefix(kv, "PATH="); ok {
				for _, dir := range filepath.SplitList(p) {
					cand := filepath.Join(dir, argv[0])
					if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
						cmd.Path = cand
						cmd.Err = nil
						break
					}
				}
			}
		}
	}
	return cmd
}

func (s *Shim) runInit(env []string) (int, error) {
	s.out.Event(proto.EvInit, map[string]any{"phase": "start"})
	cmd := s.command([]string{"/bin/sh", "-c", s.cfg.Init}, env)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	s.mu.Lock()
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return -1, err
	}
	s.initPid = cmd.Process.Pid
	s.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyTo(stdout, func(b []byte) { s.out.Write("stdout", b) }) }()
	go func() { defer wg.Done(); copyTo(stderr, func(b []byte) { s.out.Write("stderr", b) }) }()
	ws := <-s.initCh
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	wg.Wait()
	s.mu.Lock()
	s.initPid = 0
	s.mu.Unlock()
	code := ws.ExitStatus()
	if ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	s.out.Event(proto.EvInit, map[string]any{"phase": "done", "exitCode": code})
	return code, nil
}

func (s *Shim) startWorkload(argv []string, env []string) (*adapter.Process, error) {
	cmd := s.command(argv, env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	s.workPid = cmd.Process.Pid
	s.proc = &adapter.Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}
	return s.proc, nil
}

func copyTo(r io.Reader, f func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			f(append([]byte{}, buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// sink connects an adapter to the output file.
type sink struct{ s *Shim }

func (k *sink) Stdout(p []byte)            { k.s.out.Write("stdout", p) }
func (k *sink) Stderr(p []byte)            { k.s.out.Write("stderr", p) }
func (k *sink) Event(typ string, data any) { k.s.out.Event(typ, data) }
func (k *sink) Session(id string)          { k.s.out.Event(proto.EvSession, map[string]string{"sessionId": id}) }
func (k *sink) Activity(idle bool) {
	a := "busy"
	if idle {
		a = "idle"
	}
	k.s.out.Event(proto.EvActivity, map[string]string{"activity": a})
}
func (k *sink) InputAck(requestID string, err error) {
	if requestID == "" {
		return
	}
	d := map[string]string{"requestId": requestID}
	if err != nil {
		d["error"] = err.Error()
	}
	k.s.out.Event(proto.EvInputAck, d)
}

// ---- users ------------------------------------------------------------------

type userInfo struct {
	name     string
	uid, gid int
	home     string
	groups   []uint32
}

// lookupUser resolves "name", "uid" or "uid:gid" against the image's
// /etc/passwd. Empty means root.
func lookupUser(spec string) (*userInfo, error) {
	if spec == "" || spec == "root" || spec == "0" {
		return &userInfo{name: "root", home: "/root"}, nil
	}
	name, group, _ := strings.Cut(spec, ":")
	u := &userInfo{name: name, home: "/"}
	found := false
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) < 7 {
				continue
			}
			if f[0] == name || f[2] == name {
				u.name = f[0]
				u.uid, _ = strconv.Atoi(f[2])
				u.gid, _ = strconv.Atoi(f[3])
				u.home = f[5]
				found = true
				break
			}
		}
	}
	if !found {
		n, err := strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("user %q is not in the image's /etc/passwd", name)
		}
		u.uid, u.gid = n, n
	}
	if group != "" {
		g, err := strconv.Atoi(group)
		if err != nil {
			return nil, errors.New("group must be numeric")
		}
		u.gid = g
	}
	u.groups = []uint32{uint32(u.gid)}
	return u, nil
}
