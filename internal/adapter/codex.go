package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/marcioapm/lux/internal/proto"
)

// Codex drives `codex app-server`: JSON-RPC 2.0, newline-delimited, on
// stdio (docs/agent-protocols.md). A thread is the conversation (persisted
// as a rollout under ~/.codex); a turn is one exchange. Mid-turn input is
// native (turn/steer); interrupt is turn/interrupt; a resume on another
// host is thread/resume with the stored thread id.
type Codex struct {
	lw      lineWriter
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	proc    *Process
	sink    Sink
	thread  string
	turn    string // in-progress turn id, "" when idle
	queue   []proto.Input
	ready   bool
	stopped bool
}

func NewCodex() *Codex { return &Codex{pending: map[int64]chan rpcResponse{}} }

func (c *Codex) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	base := cfg.Command
	if len(base) == 0 {
		base = []string{"codex"}
	}
	return append(append([]string{}, base...), "app-server"), nil
}

func (c *Codex) call(method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.lw.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	r := <-ch
	if r.Err != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Err.Message)
	}
	return r.Result, nil
}

func (c *Codex) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	c.mu.Lock()
	c.proc, c.sink = p, sink
	c.lw.w = p.Stdin
	c.mu.Unlock()
	go pump(p.Stderr, sink.Stderr)
	done := make(chan struct{})
	go func() { defer close(done); c.readLoop(p, sink) }()

	if err := c.handshake(cfg, sink); err != nil {
		sink.Event(proto.EvWarning, map[string]any{"message": "codex: " + err.Error()})
		_ = p.Signal(syscall.SIGTERM)
	} else {
		c.mu.Lock()
		c.ready = true
		c.mu.Unlock()
		c.drain()
	}
	<-done
	c.mu.Lock()
	for id, ch := range c.pending {
		ch <- rpcResponse{Err: &rpcError{Message: "codex exited"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return nil
}

func (c *Codex) handshake(cfg proto.ShimConfig, sink Sink) error {
	if _, err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "lux", "version": "1"}}); err != nil {
		return err
	}
	cwd := cfg.Workdir
	if cwd == "" {
		cwd = home()
	}
	var res json.RawMessage
	var err error
	if cfg.Resume && cfg.SessionID != "" {
		res, err = c.call("thread/resume", map[string]any{"threadId": cfg.SessionID})
		if err != nil {
			sink.Event(proto.EvWarning, map[string]any{"message": "thread/resume failed, starting a new thread: " + err.Error()})
		}
	}
	if res == nil {
		res, err = c.call("thread/start", map[string]any{"cwd": cwd})
		if err != nil {
			return err
		}
		if !cfg.Resume && cfg.Prompt != "" {
			c.mu.Lock()
			c.queue = append([]proto.Input{{RequestID: "prompt", Text: cfg.Prompt}}, c.queue...)
			c.mu.Unlock()
		}
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Thread.ID == "" {
		return errors.New("no thread id")
	}
	c.mu.Lock()
	c.thread = r.Thread.ID
	c.mu.Unlock()
	sink.Session(r.Thread.ID)
	sink.Activity(true)
	return nil
}

func (c *Codex) drain() {
	c.mu.Lock()
	if !c.ready || c.stopped || c.turn != "" || len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}
	in := c.queue[0]
	c.queue = c.queue[1:]
	c.turn = "starting"
	thread := c.thread
	sink := c.sink
	c.mu.Unlock()
	sink.Activity(false)
	go func() {
		res, err := c.call("turn/start", map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": in.Text}}})
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(res, &r)
		c.mu.Lock()
		if err != nil {
			c.turn = ""
		} else if c.turn == "starting" {
			c.turn = r.Turn.ID
		}
		c.mu.Unlock()
		if in.RequestID != "prompt" {
			sink.InputAck(in.RequestID, err)
		}
		if err != nil {
			sink.Activity(true)
			c.drain()
		}
	}()
}

func (c *Codex) readLoop(p *Process, sink Sink) {
	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			sink.Stdout(append(append([]byte{}, line...), '\n'))
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			// Server requests (approvals): unattended, so approve; the
			// sandbox is the container.
			_ = c.lw.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]string{"decision": "approved"}})
			sink.Event("codex.request."+m.Method, json.RawMessage(m.Params))
		case m.Method != "":
			c.handleNotification(m, sink)
		case len(m.ID) > 0:
			var id int64
			_ = json.Unmarshal(m.ID, &id)
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcResponse{Result: m.Result, Err: m.Error}
			}
		}
	}
}

func (c *Codex) handleNotification(m rpcMsg, sink Sink) {
	switch m.Method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		c.turn = p.Turn.ID
		c.mu.Unlock()
	case "turn/completed":
		c.mu.Lock()
		c.turn = ""
		idle := len(c.queue) == 0
		c.mu.Unlock()
		if idle {
			sink.Activity(true)
		}
		defer c.drain()
	case "item/completed":
		var p struct {
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Item.Type == "agentMessage" && p.Item.Text != "" {
			sink.Stdout([]byte(p.Item.Text + "\n"))
		}
	}
	sink.Event("codex."+m.Method, json.RawMessage(m.Params))
}

// Deliver steers a running turn natively (turn/steer), or starts a turn.
func (c *Codex) Deliver(in proto.Input) {
	c.mu.Lock()
	turn, thread, ready := c.turn, c.thread, c.ready
	c.mu.Unlock()
	if !ready || turn == "" || turn == "starting" {
		c.mu.Lock()
		c.queue = append(c.queue, in)
		c.mu.Unlock()
		c.drain()
		return
	}
	if in.Interrupt {
		_, _ = c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
		c.mu.Lock()
		c.queue = append([]proto.Input{in}, c.queue...)
		c.mu.Unlock()
		return
	}
	go func() {
		_, err := c.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn,
			"input": []map[string]string{{"type": "text", "text": in.Text}}})
		if err != nil {
			// The turn ended meanwhile: send it as the next turn.
			c.mu.Lock()
			c.queue = append(c.queue, in)
			c.mu.Unlock()
			c.drain()
			return
		}
		c.sink.InputAck(in.RequestID, nil)
	}()
}

func (c *Codex) Interrupt() error {
	c.mu.Lock()
	turn, thread := c.turn, c.thread
	c.mu.Unlock()
	if turn == "" || turn == "starting" {
		return nil
	}
	_, err := c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

// Stop interrupts the turn (so the rollout is flushed) and closes stdin.
func (c *Codex) Stop() error {
	c.mu.Lock()
	c.stopped = true
	p := c.proc
	c.mu.Unlock()
	_ = c.Interrupt()
	if p != nil && p.Stdin != nil {
		c.lw.mu.Lock()
		_ = p.Stdin.Close()
		c.lw.w = nil
		c.lw.mu.Unlock()
	}
	return nil
}
