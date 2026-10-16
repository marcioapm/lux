package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marcioapm/lux/internal/proto"
)

// A tunnel's output waits for credit: a window's worth, then only as luxd
// grants more. Without a window, nothing waits.
func TestStreamCredit(t *testing.T) {
	if newCredit(0) != nil {
		t.Fatal("no window, yet flow control")
	}
	var none *credit
	if err := none.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := newCredit(2)
	ctx := context.Background()
	for range 2 {
		if err := c.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.take(short); err == nil {
		t.Fatal("took past the window")
	}
	done := make(chan error)
	go func() { done <- c.take(ctx) }()
	time.Sleep(20 * time.Millisecond)
	c.grant(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a grant did not wake the taker")
	}
}

// streamRig is a Runner with one running placement whose shim socket is
// served by the test (shim), and a luxd WebSocket whose received stream
// frames arrive on frames.
type streamRig struct {
	r      *Runner
	shim   net.Listener
	frames chan proto.Frame
}

func newStreamRig(t *testing.T) *streamRig {
	t.Helper()
	// A unix socket path is limited to 108 bytes: t.TempDir's may be longer.
	dir, err := os.MkdirTemp("", "lux-stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	shim, err := net.Listen("unix", filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shim.Close() })

	frames := make(chan proto.Frame, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			var f proto.Frame
			if readFrame(req.Context(), ws, &f) != nil {
				return
			}
			frames <- f
		}
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })

	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		placements: map[string]*placement{}, streams: streams{m: map[string]*stream{}}}
	r.conn = newConn(r)
	r.conn.ws = ws
	r.mounts.Store(runtimeVolume("run_s"), dir)
	r.placements["run_s"] = &placement{r: r, runID: "run_s", epoch: 1, phase: "running"}
	return &streamRig{r: r, shim: shim, frames: frames}
}

// open starts an exec stream as handleStreamFrame does, with an unbuffered
// input: a send into it returns only once the stream's input goroutine has
// finished writing the frame before. It returns once the fake shim has the
// stream's connection and its ShimStream line.
func (g *streamRig) open(t *testing.T) (*stream, *net.UnixConn, <-chan struct{}) {
	t.Helper()
	sctx, cancel := context.WithCancelCause(context.Background())
	st := &stream{input: make(chan proto.StreamData), cancel: cancel}
	g.r.streams.put("str_1", st)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		g.r.runStream(sctx, "run_s", 1, "str_1", proto.StreamOpen{Kind: "exec", Command: []string{"true"}}, st)
	}()
	c, err := g.shim.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m proto.ShimMsg
	if err := json.Unmarshal(line, &m); err != nil || m.Type != proto.ShimStream {
		t.Fatalf("shim got %q, want a ShimStream", line)
	}
	return st, c.(*net.UnixConn), ended
}

// closes waits for the stream to end and returns every stream.close luxd
// received for it.
func (g *streamRig) closes(t *testing.T, ended <-chan struct{}) []proto.StreamData {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never ended")
	}
	// Frames are written before runStream returns; a ping on the same
	// connection, received after them, says they have all arrived.
	if err := g.r.conn.Send(context.Background(), proto.Frame{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var out []proto.StreamData
	for {
		select {
		case f := <-g.frames:
			if f.Type == "ping" {
				return out
			}
			if f.Type == proto.MsgStreamClose {
				var d proto.StreamData
				if err := json.Unmarshal(f.Data, &d); err != nil {
					t.Fatal(err)
				}
				out = append(out, d)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("luxd's frames never arrived")
		}
	}
}

// feed sends d into the stream's input; false if the stream ended first.
func feed(st *stream, ended <-chan struct{}, d proto.StreamData) bool {
	select {
	case st.input <- d:
		return true
	case <-ended:
		return false
	case <-time.After(10 * time.Second):
		return false
	}
}

// An exec'd command that exits without reading its stdin: the shim stops
// reading the stream's connection, and the client's EOF (and any input)
// fails to be written. That is not the stream's end: the shim's exit line
// is, and it reaches luxd as the one stream.close.
func TestStreamExecExitsBeforeItsInput(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t)

	// The shim stops reading: every write to it now fails (EPIPE).
	if err := shim.CloseRead(); err != nil {
		t.Fatal(err)
	}
	// The second feed returns once the first frame's write has failed; the
	// third, once the second's has.
	for _, d := range []proto.StreamData{{EOF: true}, {Data: []byte("late")}, {Data: []byte("later")}} {
		if !feed(st, ended, d) {
			break
		}
	}
	if _, err := shim.Write([]byte(`{"exitCode":0}` + "\n")); err == nil {
		shim.Close()
	}

	closes := g.closes(t, ended)
	if len(closes) != 1 {
		t.Fatalf("%d stream.close frames %+v, want 1", len(closes), closes)
	}
	if c := closes[0]; c.Error != "" || c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("stream.close %+v, want exit code 0 and no error", c)
	}
}

// The same with the real shim's order: the exit line written and the
// connection closed before the input arrives.
func TestStreamExecExitedAndClosedBeforeItsInput(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t)
	if _, err := shim.Write([]byte(`{"exitCode":0}` + "\n")); err != nil {
		t.Fatal(err)
	}
	shim.Close()
	for _, d := range []proto.StreamData{{EOF: true}, {Data: []byte("late")}} {
		if !feed(st, ended, d) {
			break
		}
	}

	closes := g.closes(t, ended)
	if len(closes) != 1 {
		t.Fatalf("%d stream.close frames %+v, want 1", len(closes), closes)
	}
	if c := closes[0]; c.Error != "" || c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("stream.close %+v, want exit code 0 and no error", c)
	}
}

// A shim that goes away mid-stream, with no exit line, still ends the
// stream with an error, input written to it or not.
func TestStreamShimGoneMidStream(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t)
	if _, err := shim.Write([]byte(`{"channel":"stdout","data":"aGk="}` + "\n")); err != nil {
		t.Fatal(err)
	}
	shim.Close()
	for _, d := range []proto.StreamData{{Data: []byte("more")}, {EOF: true}} {
		if !feed(st, ended, d) {
			break
		}
	}

	closes := g.closes(t, ended)
	if len(closes) != 1 {
		t.Fatalf("%d stream.close frames %+v, want 1", len(closes), closes)
	}
	if c := closes[0]; c.ExitCode != nil || c.Error == "" {
		t.Fatalf("stream.close %+v, want an error and no exit code", c)
	}
}
