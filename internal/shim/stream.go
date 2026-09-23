package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
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
// only that stream, as proto.StreamData JSON lines: stdin, EOF or a resize
// from the runner; output and finally one with ExitCode or Error from the
// shim. Closing the connection ends the stream (an exec'd process is
// killed; an attach just detaches).
//
// Streams are for a person at a terminal. They are not recorded in the
// Run's output (an attached workload's terminal output is, as always).

// streamConn serializes messages to one stream's connection.
type streamConn struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (c *streamConn) send(d proto.StreamData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(d)
}

func (c *streamConn) fail(msg string) { _ = c.send(proto.StreamData{Error: msg}) }

func (s *Shim) handleStream(sc *bufio.Scanner, enc *json.Encoder, open proto.StreamOpen) {
	out := &streamConn{enc: enc}
	switch open.Kind {
	case "exec":
		s.execStream(sc, out, open)
	case "attach":
		s.attachStream(sc, out)
	default:
		out.fail("unknown stream kind " + open.Kind)
	}
}

// execStream runs a command in the container as the workload user, with
// its environment, under a PTY if asked.
func (s *Shim) execStream(sc *bufio.Scanner, out *streamConn, open proto.StreamOpen) {
	s.mu.Lock()
	env := s.env
	s.mu.Unlock()
	if env == nil {
		out.fail("the workload has not started yet")
		return
	}
	if len(open.Command) == 0 {
		out.fail("no command")
		return
	}
	cmd := s.command(open.Command, env)
	type output struct {
		ch string
		r  io.Reader
	}
	var stdin io.WriteCloser
	var ptmx *os.File
	var outputs []output
	var exited chan syscall.WaitStatus
	var err error
	if open.TTY {
		// A PTY makes the command a session leader: Setsid, not Setpgid.
		cmd.SysProcAttr.Setpgid = false
		exited, err = s.startTracked(func() error {
			var e error
			ptmx, e = pty.StartWithSize(cmd, winsize(open.Rows, open.Cols))
			return e
		}, &cmd.Process)
		stdin, outputs = ptmx, []output{{"stdout", ptmx}}
	} else {
		in, e1 := cmd.StdinPipe()
		o, e2 := cmd.StdoutPipe()
		e, e3 := cmd.StderrPipe()
		if err = errors.Join(e1, e2, e3); err == nil {
			exited, err = s.startTracked(cmd.Start, &cmd.Process)
		}
		stdin, outputs = in, []output{{"stdout", o}, {"stderr", e}}
	}
	if err != nil {
		out.fail(err.Error())
		return
	}

	go func() {
		readStreamInput(sc, func(d proto.StreamData) {
			switch {
			case d.Rows > 0 && ptmx != nil:
				_ = pty.Setsize(ptmx, winsize(d.Rows, d.Cols))
			case d.EOF && ptmx == nil:
				_ = stdin.Close()
			case d.EOF:
				_, _ = stdin.Write([]byte{4}) // ^D
			case len(d.Data) > 0:
				_, _ = stdin.Write(d.Data)
			}
		})
		// The client went away: so does the command (it leads its own
		// process group, or session under a PTY).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()

	var wg sync.WaitGroup
	for _, o := range outputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyTo(o.r, func(b []byte) { _ = out.send(proto.StreamData{Channel: o.ch, Data: b}) })
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
	// The reaper collected the process, so cmd.Wait (which would close
	// the pipes' parent ends) is never called: close them here.
	_ = stdin.Close()
	for _, o := range outputs {
		if c, ok := o.r.(io.Closer); ok {
			_ = c.Close()
		}
	}
	code := exitCode(ws)
	_ = out.send(proto.StreamData{ExitCode: &code})
}

// attachStream joins the workload's terminal (a generic workload started
// with workload.tty): its output from now on, and input into it.
func (s *Shim) attachStream(sc *bufio.Scanner, out *streamConn) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		out.fail("the workload has no terminal: attach needs workload.tty (generic workloads)")
		return
	}
	sub := term.join()
	defer term.leave(sub)
	go func() {
		for b := range sub {
			_ = out.send(proto.StreamData{Channel: "stdout", Data: b})
		}
	}()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		readStreamInput(sc, func(d proto.StreamData) {
			switch {
			case d.Rows > 0:
				_ = pty.Setsize(term.ptmx, winsize(d.Rows, d.Cols))
			case len(d.Data) > 0:
				_, _ = term.ptmx.Write(d.Data)
			}
		})
	}()
	select {
	case <-inputDone: // detached
	case <-term.gone: // the workload exited
		_ = out.send(proto.StreamData{Error: "the workload exited"})
	}
}

// terminal is a workload's PTY, read for the output file and copied to
// whoever is attached. An attached client that cannot keep up misses
// output; it never holds up the workload or its output file.
type terminal struct {
	ptmx     *os.File
	mu       sync.Mutex
	subs     map[chan []byte]bool
	gone     chan struct{}
	goneOnce sync.Once
}

func newTerminal(ptmx *os.File) *terminal {
	return &terminal{ptmx: ptmx, subs: map[chan []byte]bool{}, gone: make(chan struct{})}
}

func (t *terminal) join() chan []byte {
	ch := make(chan []byte, 256)
	t.mu.Lock()
	t.subs[ch] = true
	t.mu.Unlock()
	return ch
}

func (t *terminal) leave(ch chan []byte) {
	t.mu.Lock()
	delete(t.subs, ch)
	t.mu.Unlock()
	close(ch)
}

// Read reads the PTY and copies what it read to everyone attached.
func (t *terminal) Read(p []byte) (int, error) {
	n, err := t.ptmx.Read(p)
	if n > 0 {
		t.mu.Lock()
		if len(t.subs) > 0 {
			b := append([]byte{}, p[:n]...)
			for ch := range t.subs {
				select {
				case ch <- b:
				default:
				}
			}
		}
		t.mu.Unlock()
	}
	if err != nil {
		t.goneOnce.Do(func() { close(t.gone) })
	}
	return n, err
}

func readStreamInput(sc *bufio.Scanner, fn func(proto.StreamData)) {
	for sc.Scan() {
		var d proto.StreamData
		if json.Unmarshal(sc.Bytes(), &d) == nil {
			fn(d)
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
func (s *Shim) startTracked(start func() error, proc **os.Process) (chan syscall.WaitStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := start(); err != nil {
		return nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	s.streams[(*proc).Pid] = ch
	return ch, nil
}

// exitCode is a process's exit code, 128+signal if a signal ended it.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
