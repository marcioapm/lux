// Package adapter is how the shim starts, resumes, steers and stops a
// workload. lux itself is agent-agnostic: a Run is a process. An adapter
// only translates between that process's own stdio protocol and lux's four
// verbs — deliver input, interrupt, stop, and report what it learned
// (session id, idle/busy, structured events).
//
//	generic      a plain process: stdin gets raw input, stdout/stderr are
//	             output, SIGINT interrupts. Works for anything.
//	acp          any Agent Client Protocol agent (JSON-RPC over stdio).
//	claude-code  Claude Code's stream-json mode.
//	codex        Codex's app-server (JSON-RPC over stdio).
//	opencode     OpenCode over ACP, with its own default command.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/marcioapm/lux/internal/proto"
)

// Sink receives everything an adapter produces. Implemented by the shim,
// which redacts, sequences and writes records.
type Sink interface {
	Stdout(p []byte)
	// EndMessage ends a message the agent wrote to stdout (in one piece or
	// streamed in several) on its own line.
	EndMessage()
	Stderr(p []byte)
	// Event writes a structured event record (ch=event).
	Event(typ string, data any)
	Session(id string)
	Activity(idle bool)
	InputAck(requestID string, err error)
}

// Process is the workload process the shim started for an adapter.
type Process struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

func (p *Process) Signal(sig syscall.Signal) error {
	if p.Cmd.Process == nil {
		return nil
	}
	// The workload runs in its own process group so a signal reaches its
	// children too.
	return syscall.Kill(-p.Cmd.Process.Pid, sig)
}

// Adapter drives one workload process.
type Adapter interface {
	// Command returns the argv to exec: first start or resume.
	Command(cfg proto.ShimConfig) ([]string, error)
	// Attach begins speaking the protocol on the started process. It
	// returns when the process's stdout closes.
	Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error
	// Deliver hands input to the workload, queueing it if the workload
	// cannot take it now. Must not block on the workload.
	Deliver(in proto.Input)
	Interrupt() error
	// Stop asks the workload to wind down gracefully. The shim SIGKILLs
	// after the grace period.
	Stop() error
}

// CredentialFiles is implemented by adapters whose agent reads its key
// from a file rather than the environment. Given the Run's secret values
// and the workload user's home, it returns the files to write (path →
// contents). The shim writes them like file secrets: on the secrets tmpfs,
// linked into place, never snapshotted.
type CredentialFiles interface {
	CredentialFiles(secrets map[string]string, home string) map[string][]byte
}

func New(name string) (Adapter, error) {
	switch name {
	case "generic", "":
		return &Generic{}, nil
	case "acp", "opencode":
		return NewACP(), nil
	case "claude-code":
		return NewClaude(), nil
	case "codex":
		return NewCodex(), nil
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}

// ---- generic --------------------------------------------------------------

// Generic runs a command as-is. Input is raw bytes (or text plus a newline)
// written to its stdin; interrupt is SIGINT; stop is SIGTERM.
type Generic struct {
	mu    sync.Mutex
	proc  *Process
	queue []proto.Input
	sink  Sink
}

func (g *Generic) Command(cfg proto.ShimConfig) ([]string, error) { return command(cfg) }

func (g *Generic) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	g.mu.Lock()
	g.proc, g.sink = p, sink
	queued := g.queue
	g.queue = nil
	g.mu.Unlock()
	for _, in := range queued {
		g.Deliver(in)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(p.Stdout, sink.Stdout) }()
	go func() { defer wg.Done(); pump(p.Stderr, sink.Stderr) }()
	wg.Wait()
	return nil
}

func (g *Generic) Deliver(in proto.Input) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		g.queue = append(g.queue, in)
		return
	}
	if in.Interrupt {
		_ = g.proc.Signal(syscall.SIGINT)
	}
	data := in.Raw
	if len(data) == 0 && in.Text != "" {
		data = []byte(in.Text + "\n")
	}
	var err error
	if len(data) > 0 && g.proc.Stdin != nil {
		_, err = g.proc.Stdin.Write(data)
	}
	g.sink.InputAck(in.RequestID, err)
}

func (g *Generic) Interrupt() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		return nil
	}
	return g.proc.Signal(syscall.SIGINT)
}

func (g *Generic) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		return nil
	}
	return g.proc.Signal(syscall.SIGTERM)
}

func pump(r io.Reader, out func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			out(b)
		}
		if err != nil {
			return
		}
	}
}

// ---- helpers --------------------------------------------------------------

// lineWriter serializes JSON lines to a process's stdin.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) set(w io.Writer) {
	l.mu.Lock()
	l.w = w
	l.mu.Unlock()
}

func (l *lineWriter) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return fmt.Errorf("process stdin is closed")
	}
	_, err = l.w.Write(append(b, '\n'))
	return err
}

// close closes stdin; later sends fail. Protocol agents exit when their
// client goes away.
func (l *lineWriter) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.w.(io.Closer); ok {
		_ = c.Close()
	}
	l.w = nil
}

// workdir is where an agent session is rooted: the spec's workdir, or the
// workload user's home.
func workdir(cfg proto.ShimConfig) string {
	if cfg.Workdir != "" {
		return cfg.Workdir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// textInput is the one-text-block message both ACP and Codex take.
func textInput(s string) []map[string]string {
	return []map[string]string{{"type": "text", "text": s}}
}

// command picks the argv: the resume command on resume if the spec has one,
// else the spec's command (spec.Normalize fills each adapter's default).
func command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("%s: no command", cfg.Adapter)
	}
	return cfg.Command, nil
}
