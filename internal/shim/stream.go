package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/marcioapm/lux/internal/proto"
)

// Interactive streams: exec and attach.
//
// The runner opens a connection on the shim socket per stream and sends
// ShimStream with a StreamOpen first. From then on the connection carries
// only that stream, as JSON lines: ShimData (stdin, EOF or a resize) from
// the runner; ShimData (output) and finally ShimExit from the shim.
// Closing the connection ends the stream (an exec'd process is killed; an
// attach just detaches).
//
// Streams are for a person at a terminal. They are not recorded in the
// Run's output (an attached workload's terminal output is, as always).

// streamConn serializes messages to one stream's connection.
type streamConn struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (c *streamConn) send(m proto.ShimMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(m)
}

func (s *Shim) handleStream(sc *bufio.Scanner, enc *json.Encoder, open proto.StreamOpen) {
	out := &streamConn{enc: enc}
	switch open.Kind {
	case "exec":
		s.execStream(sc, out, open)
	case "attach":
		s.attachStream(sc, out)
	default:
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, Error: "unknown stream kind " + open.Kind})
	}
}

// execStream runs a command in the container as the workload user, with
// its environment, under a PTY if asked.
func (s *Shim) execStream(sc *bufio.Scanner, out *streamConn, open proto.StreamOpen) {
	s.mu.Lock()
	env, user := s.env, s.user
	s.mu.Unlock()
	if user == nil {
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, Error: "the workload has not started yet"})
		return
	}
	if len(open.Command) == 0 {
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, Error: "no command"})
		return
	}
	cmd := s.command(open.Command, env)
	var stdin io.WriteCloser
	var ptmx *os.File
	var readers []io.Reader
	var exited chan syscall.WaitStatus
	var err error
	if open.TTY {
		// A PTY makes the command a session leader: Setsid, not Setpgid.
		cmd.SysProcAttr.Setpgid = false
		exited, err = s.startTracked(func() error {
			var e error
			ptmx, e = pty.StartWithSize(cmd, winsize(open.Rows, open.Cols))
			return e
		}, cmd)
		stdin, readers = ptmx, []io.Reader{ptmx}
	} else {
		in, e1 := cmd.StdinPipe()
		o, e2 := cmd.StdoutPipe()
		e, e3 := cmd.StderrPipe()
		if err = errors.Join(e1, e2, e3); err == nil {
			exited, err = s.startTracked(cmd.Start, cmd)
		}
		stdin, readers = in, []io.Reader{o, e}
	}
	if err != nil {
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, Error: err.Error()})
		return
	}

	go func() {
		readStreamInput(sc, func(m proto.ShimMsg) {
			switch {
			case m.Rows > 0 && ptmx != nil:
				_ = pty.Setsize(ptmx, winsize(m.Rows, m.Cols))
			case m.EOF && ptmx == nil:
				_ = stdin.Close()
			case m.EOF:
				_, _ = stdin.Write([]byte{4}) // ^D
			case len(m.Data) > 0:
				_, _ = stdin.Write(m.Data)
			}
		})
		// The client went away: so does the command.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}()

	var wg sync.WaitGroup
	for i, r := range readers {
		ch := "stdout"
		if i == 1 {
			ch = "stderr"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyOut(r, func(b []byte) { _ = out.send(proto.ShimMsg{Type: proto.ShimData, Channel: ch, Data: b}) })
		}()
	}
	ws := <-exited
	// What the process wrote before exiting is still being read. A PTY
	// may be held open by its background children: do not wait forever.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if ptmx != nil {
		_ = ptmx.Close()
	}
	code := ws.ExitStatus()
	if ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	_ = out.send(proto.ShimMsg{Type: proto.ShimExit, ExitCode: &code})
}

// attachStream joins the workload's terminal (a generic workload started
// with workload.tty): its output from now on, and input into it.
func (s *Shim) attachStream(sc *bufio.Scanner, out *streamConn) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, Error: "the workload has no terminal: attach needs workload.tty (generic workloads)"})
		return
	}
	id, gone := term.join(func(b []byte) { _ = out.send(proto.ShimMsg{Type: proto.ShimData, Channel: "stdout", Data: b}) })
	defer term.leave(id)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		readStreamInput(sc, func(m proto.ShimMsg) {
			switch {
			case m.Rows > 0:
				_ = pty.Setsize(term.ptmx, winsize(m.Rows, m.Cols))
			case len(m.Data) > 0:
				_, _ = term.ptmx.Write(m.Data)
			}
		})
	}()
	select {
	case <-inputDone: // detached
	case <-gone: // the workload exited
		code := 0
		_ = out.send(proto.ShimMsg{Type: proto.ShimExit, ExitCode: &code})
	}
}

// terminal is a workload's PTY, shared by the output file and by whoever
// is attached.
type terminal struct {
	ptmx *os.File
	mu   sync.Mutex
	subs map[int]func([]byte)
	next int
	gone chan struct{}
}

func newTerminal(ptmx *os.File) *terminal {
	return &terminal{ptmx: ptmx, subs: map[int]func([]byte){}, gone: make(chan struct{})}
}

func (t *terminal) join(fn func([]byte)) (int, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.subs[t.next] = fn
	return t.next, t.gone
}

func (t *terminal) leave(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subs, id)
}

// Read reads the PTY and copies what it read to everyone attached.
func (t *terminal) Read(p []byte) (int, error) {
	n, err := t.ptmx.Read(p)
	if n > 0 {
		b := append([]byte{}, p[:n]...)
		t.mu.Lock()
		for _, fn := range t.subs {
			fn(b)
		}
		t.mu.Unlock()
	}
	if err != nil {
		t.mu.Lock()
		select {
		case <-t.gone:
		default:
			close(t.gone)
		}
		t.mu.Unlock()
	}
	return n, err
}

func readStreamInput(sc *bufio.Scanner, fn func(proto.ShimMsg)) {
	for sc.Scan() {
		var m proto.ShimMsg
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			fn(m)
		}
	}
}

func copyOut(r io.Reader, fn func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			fn(append([]byte{}, buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

func winsize(rows, cols int) *pty.Winsize {
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

// startTracked starts a process with the reaper told to hand its exit to
// the returned channel. The lock is held across the start, so the reaper
// cannot reap the process before it is registered.
func (s *Shim) startTracked(start func() error, cmd *exec.Cmd) (chan syscall.WaitStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := start(); err != nil {
		return nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	s.streams[cmd.Process.Pid] = ch
	return ch, nil
}
