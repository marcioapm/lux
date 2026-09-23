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
// stream.data (input) and stream.close, each Frame naming its stream; the
// runner answers with stream.data (output) and one stream.close (with the
// exit code, or an error). Exec and attach go to the shim, one socket
// connection per stream, carrying StreamData lines; a tunnel is a TCP
// connection to a declared port on the Run's container.
//
// Each stream has its own goroutine writing its input in order. Input
// waits in a bounded buffer; a stream whose target stops reading loses
// the stream rather than growing the runner's memory or holding up
// other streams. A close is never queued behind input.
type stream struct {
	input  chan proto.StreamData
	cancel context.CancelFunc
}

// streamInputBuffer is how many input frames a stream holds while its
// target is slow to read them.
const streamInputBuffer = 256

type streams struct {
	mu sync.Mutex
	m  map[string]*stream
}

func (ss *streams) get(id string) *stream {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.m[id]
}

func (ss *streams) put(id string, st *stream) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.m[id] = st
}

func (ss *streams) remove(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, id)
}

// handleStreamFrame is called on the connection's read loop: it must not
// block.
func (r *Runner) handleStreamFrame(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgStreamOpen:
		var o proto.StreamOpen
		if json.Unmarshal(f.Data, &o) != nil {
			return
		}
		// Registered before anything else arrives for it: the frames of a
		// connection are read in order.
		sctx, cancel := context.WithCancel(ctx)
		st := &stream{input: make(chan proto.StreamData, streamInputBuffer), cancel: cancel}
		r.streams.put(f.Stream, st)
		go r.runStream(sctx, f.RunID, f.Epoch, f.Stream, o, st)
	case proto.MsgStreamData:
		st := r.streams.get(f.Stream)
		if st == nil {
			return
		}
		var d proto.StreamData
		if json.Unmarshal(f.Data, &d) != nil {
			return
		}
		select {
		case st.input <- d:
		default:
			st.cancel() // its target is not reading: end the stream
		}
	case proto.MsgStreamClose:
		if st := r.streams.get(f.Stream); st != nil {
			st.cancel()
		}
	}
}

func (r *Runner) runStream(ctx context.Context, runID string, epoch int, id string, o proto.StreamOpen, st *stream) {
	defer r.streams.remove(id)
	defer st.cancel()
	send := func(typ string, data []byte) {
		_ = r.conn.Send(context.WithoutCancel(ctx), proto.Frame{Type: typ, RunID: runID, Epoch: epoch, Stream: id, Data: data})
	}
	closeWith := func(d proto.StreamData) { send(proto.MsgStreamClose, proto.Marshal(d)) }

	r.mu.Lock()
	p := r.placements[runID]
	r.mu.Unlock()
	if p == nil || p.epoch != epoch || p.liveState() != "running" {
		closeWith(proto.StreamData{Error: "the Run is not running here"})
		return
	}
	var conn net.Conn
	var err error
	switch o.Kind {
	case "exec", "attach":
		conn, err = p.shimStream(ctx, o)
	case "tunnel":
		conn, err = p.dialPort(ctx, o.Port)
	default:
		err = fmt.Errorf("unknown stream kind %q", o.Kind)
	}
	if err != nil {
		closeWith(proto.StreamData{Error: err.Error()})
		return
	}
	go func() { <-ctx.Done(); conn.Close() }()

	// Input, in order.
	go func() {
		enc := json.NewEncoder(conn)
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-st.input:
				var err error
				switch {
				case o.Kind != "tunnel":
					err = enc.Encode(d)
				case d.EOF:
					if tc, ok := conn.(*net.TCPConn); ok {
						err = tc.CloseWrite()
					}
				default:
					_, err = conn.Write(d.Data)
				}
				if err != nil {
					st.cancel()
					return
				}
			}
		}
	}()

	// Output.
	if o.Kind == "tunnel" {
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				send(proto.MsgStreamData, proto.Marshal(proto.StreamData{Data: buf[:n]}))
			}
			if err != nil {
				closeWith(proto.StreamData{})
				return
			}
		}
	}
	// The shim's lines are StreamData already: forwarded as they are.
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := append([]byte{}, sc.Bytes()...)
		var last struct {
			ExitCode *int   `json:"exitCode"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(line, &last)
		if last.ExitCode != nil || last.Error != "" {
			send(proto.MsgStreamClose, line)
			return
		}
		send(proto.MsgStreamData, line)
	}
	if ctx.Err() == nil {
		closeWith(proto.StreamData{Error: "the container went away"})
	}
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
	ip, err := p.containerIP(ctx)
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

// containerIP is the container's address on its network, looked up once:
// it does not change while the container lives.
func (p *placement) containerIP(ctx context.Context) (string, error) {
	p.mu.Lock()
	ip := p.ip
	p.mu.Unlock()
	if ip != "" {
		return ip, nil
	}
	ip, err := p.r.pm.ContainerIP(ctx, containerName(p.runID), networkName(p.runID))
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.ip = ip
	p.mu.Unlock()
	return ip, nil
}
