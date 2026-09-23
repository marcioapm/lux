package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/marcioapm/lux/internal/proto"
)

// Interactive streams (exec, attach, port-forward tunnels), relayed
// between luxd and the container. luxd sends stream.open, then
// stream.data (input) and stream.close; the runner answers with
// stream.data (output) and one stream.close (with the exit code, or an
// error). Exec and attach go to the shim, one socket connection per
// stream; a tunnel is a TCP connection to a declared port on the Run's
// container.
type stream struct {
	mu      sync.Mutex
	in      func(proto.StreamData) // input from luxd, once connected
	pending []proto.StreamData     // input that came before that
	closed  bool
	cancel  func()
}

// input hands data to the stream, or keeps it until the stream is
// connected (luxd sends input right behind stream.open).
func (st *stream) input(d proto.StreamData) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.in == nil {
		st.pending = append(st.pending, d)
		return
	}
	st.in(d)
}

func (st *stream) connected(in func(proto.StreamData), cancel func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.in, st.cancel = in, cancel
	for _, d := range st.pending {
		in(d)
	}
	st.pending = nil
	if st.closed {
		cancel()
	}
}

func (st *stream) close() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.closed = true
	if st.cancel != nil {
		st.cancel()
	}
}

type streams struct {
	mu sync.Mutex
	m  map[string]*stream
}

func (r *Runner) handleStream(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgStreamOpen:
		var o proto.StreamOpen
		if json.Unmarshal(f.Data, &o) != nil {
			return
		}
		r.openStream(ctx, f.RunID, f.Epoch, o)
	case proto.MsgStreamData, proto.MsgStreamClose:
		var d proto.StreamData
		if json.Unmarshal(f.Data, &d) != nil {
			return
		}
		r.streams.mu.Lock()
		st := r.streams.m[d.StreamID]
		r.streams.mu.Unlock()
		if st == nil {
			return
		}
		if f.Type == proto.MsgStreamClose {
			st.close()
			return
		}
		st.input(d)
	}
}

func (r *Runner) openStream(ctx context.Context, runID string, epoch int, o proto.StreamOpen) {
	send := func(typ string, d proto.StreamData) {
		d.StreamID = o.StreamID
		_ = r.conn.Send(ctx, proto.Frame{Type: typ, RunID: runID, Epoch: epoch, Data: proto.Marshal(d)})
	}
	fail := func(err error) {
		send(proto.MsgStreamClose, proto.StreamData{Error: err.Error()})
	}
	// Registered first: input may follow stream.open at once.
	st := &stream{}
	r.streams.mu.Lock()
	r.streams.m[o.StreamID] = st
	r.streams.mu.Unlock()
	forget := func() {
		r.streams.mu.Lock()
		delete(r.streams.m, o.StreamID)
		r.streams.mu.Unlock()
	}
	r.mu.Lock()
	p := r.placements[runID]
	r.mu.Unlock()
	if p == nil || p.epoch != epoch || p.liveState() != "running" {
		forget()
		fail(fmt.Errorf("the Run is not running here"))
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	var conn net.Conn
	var err error
	switch o.Kind {
	case "exec", "attach":
		conn, err = p.shimStream(sctx, o)
	case "tunnel":
		conn, err = p.dialPort(sctx, o.Port)
	default:
		err = fmt.Errorf("unknown stream kind %q", o.Kind)
	}
	if err != nil {
		cancel()
		forget()
		fail(err)
		return
	}
	stop := func() { cancel(); conn.Close() }
	var in func(proto.StreamData)
	if o.Kind == "tunnel" {
		in = func(d proto.StreamData) {
			if d.EOF {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				return
			}
			_, _ = conn.Write(d.Data)
		}
	} else {
		enc := json.NewEncoder(conn)
		in = func(d proto.StreamData) {
			_ = enc.Encode(proto.ShimMsg{Type: proto.ShimData, Data: d.Data, EOF: d.EOF, Rows: d.Rows, Cols: d.Cols})
		}
	}
	st.connected(in, stop)

	go func() {
		defer func() {
			forget()
			stop()
		}()
		if o.Kind == "tunnel" {
			buf := make([]byte, 32<<10)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					send(proto.MsgStreamData, proto.StreamData{Data: append([]byte{}, buf[:n]...)})
				}
				if err != nil {
					send(proto.MsgStreamClose, proto.StreamData{})
					return
				}
			}
		}
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var m proto.ShimMsg
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m.Type {
			case proto.ShimData:
				send(proto.MsgStreamData, proto.StreamData{Data: m.Data, Channel: m.Channel})
			case proto.ShimExit:
				send(proto.MsgStreamClose, proto.StreamData{ExitCode: m.ExitCode, Error: m.Error})
				return
			}
		}
		if sctx.Err() == nil {
			send(proto.MsgStreamClose, proto.StreamData{Error: "the container went away"})
		}
	}()
}

// shimStream opens a connection to the shim for one stream.
func (p *placement) shimStream(ctx context.Context, o proto.StreamOpen) (net.Conn, error) {
	path, err := p.socketPath(ctx)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("shim: %w", err)
	}
	if err := json.NewEncoder(c).Encode(proto.ShimMsg{Type: proto.ShimStream, Stream: &o}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// dialPort connects to a port the Run's spec declares, on its container.
// Only declared ports: a tunnel is not a way into anything else.
func (p *placement) dialPort(ctx context.Context, port int) (net.Conn, error) {
	declared := false
	for _, dp := range p.assign.Spec.Network.Ports {
		declared = declared || dp.Port == port
	}
	if !declared {
		return nil, fmt.Errorf("port %d is not declared in the Run's spec", port)
	}
	ip, err := p.r.pm.ContainerIP(ctx, containerName(p.runID), networkName(p.runID))
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("port %d: %s", port, strings.TrimPrefix(err.Error(), "dial tcp "))
	}
	return c, nil
}
