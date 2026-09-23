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

// ACP speaks the Agent Client Protocol: JSON-RPC 2.0, newline-delimited, on
// the agent's stdin/stdout. One adapter for every ACP agent.
//
// ACP has no mid-turn message, so input that arrives during a turn is
// queued and sent as the next session/prompt when the turn ends; with
// interrupt it cancels the turn (session/cancel) first.
//
// Permission requests are answered by policy, since nobody is watching:
// the first allow_once option (the Run is already sandboxed; its limits are
// the container's). See docs/adapters.md.
type ACP struct {
	lw      lineWriter
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	session string
	busy    bool
	queue   []proto.Input
	current string // request id of the input being processed
	sink    Sink
	proc    *Process
	started bool
	ready   chan struct{}
	stopped bool
}

func NewACP() *ACP {
	return &ACP{pending: map[int64]chan rpcResponse{}, ready: make(chan struct{})}
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage
	Err    *rpcError
}

func (a *ACP) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	if len(cfg.Command) > 0 {
		return cfg.Command, nil
	}
	if cfg.Adapter == "opencode" {
		return []string{"opencode", "acp"}, nil
	}
	return nil, errors.New("acp: workload.command is required")
}

func (a *ACP) call(method string, params any) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.mu.Unlock()
	if err := a.lw.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	r := <-ch
	if r.Err != nil {
		return nil, fmt.Errorf("%s: %s (%d)", method, r.Err.Message, r.Err.Code)
	}
	return r.Result, nil
}

func (a *ACP) notify(method string, params any) error {
	return a.lw.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (a *ACP) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	a.mu.Lock()
	a.sink, a.proc = sink, p
	a.lw.w = p.Stdin
	a.mu.Unlock()

	go pump(p.Stderr, sink.Stderr)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		a.readLoop(p, sink)
	}()

	err := a.handshake(cfg, sink)
	if err != nil {
		sink.Event(proto.EvWarning, map[string]any{"message": "acp: " + err.Error()})
		_ = p.Signal(syscall.SIGTERM)
	} else {
		close(a.ready)
		a.drain()
	}
	<-readDone
	a.mu.Lock()
	for id, ch := range a.pending {
		ch <- rpcResponse{Err: &rpcError{Message: "agent exited"}}
		delete(a.pending, id)
	}
	a.mu.Unlock()
	return err
}

func (a *ACP) handshake(cfg proto.ShimConfig, sink Sink) error {
	res, err := a.call("initialize", map[string]any{
		"protocolVersion": 1,
		// Terminal and file-system access stay inside the agent: lux does
		// not proxy them.
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "lux", "version": "1"},
	})
	if err != nil {
		return err
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &init)
	cwd := cfg.Workdir
	if cwd == "" {
		cwd = home()
	}
	if cfg.Resume && cfg.SessionID != "" && init.AgentCapabilities.LoadSession {
		if _, err := a.call("session/load", map[string]any{"sessionId": cfg.SessionID, "cwd": cwd, "mcpServers": []any{}}); err == nil {
			a.setSession(cfg.SessionID)
			return nil
		} else {
			sink.Event(proto.EvWarning, map[string]any{"message": "session/load failed, starting a new session: " + err.Error()})
		}
	}
	res, err = a.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return err
	}
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &ns); err != nil || ns.SessionID == "" {
		return fmt.Errorf("session/new returned no sessionId")
	}
	a.setSession(ns.SessionID)
	if !cfg.Resume && cfg.Prompt != "" {
		a.mu.Lock()
		a.queue = append([]proto.Input{{RequestID: "prompt", Text: cfg.Prompt}}, a.queue...)
		a.mu.Unlock()
	}
	return nil
}

func (a *ACP) setSession(id string) {
	a.mu.Lock()
	a.session = id
	sink := a.sink
	a.mu.Unlock()
	sink.Session(id)
	sink.Activity(true)
}

// drain sends the next queued input as a prompt, if the agent is idle.
func (a *ACP) drain() {
	a.mu.Lock()
	select {
	case <-a.ready:
	default:
		a.mu.Unlock()
		return
	}
	if a.busy || len(a.queue) == 0 || a.stopped {
		a.mu.Unlock()
		return
	}
	in := a.queue[0]
	a.queue = a.queue[1:]
	a.busy = true
	a.current = in.RequestID
	session := a.session
	sink := a.sink
	a.mu.Unlock()

	sink.Activity(false)
	if in.RequestID != "prompt" {
		sink.InputAck(in.RequestID, nil)
	}
	go func() {
		res, err := a.call("session/prompt", map[string]any{
			"sessionId": session,
			"prompt":    []map[string]string{{"type": "text", "text": in.Text}},
		})
		var pr struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(res, &pr)
		data := map[string]any{"stopReason": pr.StopReason}
		if err != nil {
			data["error"] = err.Error()
		}
		sink.Event("acp.turn_end", data)
		a.mu.Lock()
		a.busy = false
		a.current = ""
		idle := len(a.queue) == 0
		a.mu.Unlock()
		if idle {
			sink.Activity(true)
		}
		a.drain()
	}()
}

func (a *ACP) readLoop(p *Process, sink Sink) {
	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC == "" {
			// Not protocol: pass through as stdout.
			sink.Stdout(append(append([]byte{}, line...), '\n'))
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			a.handleRequest(m, sink)
		case m.Method != "":
			a.handleNotification(m, sink)
		case len(m.ID) > 0:
			var id int64
			_ = json.Unmarshal(m.ID, &id)
			a.mu.Lock()
			ch := a.pending[id]
			delete(a.pending, id)
			a.mu.Unlock()
			if ch != nil {
				ch <- rpcResponse{Result: m.Result, Err: m.Error}
			}
		}
	}
}

func (a *ACP) handleNotification(m rpcMsg, sink Sink) {
	if m.Method != "session/update" {
		sink.Event("acp."+m.Method, json.RawMessage(m.Params))
		return
	}
	var p struct {
		Update json.RawMessage `json:"update"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var u struct {
		Kind    string `json:"sessionUpdate"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(p.Update, &u)
	// The agent's reply text is also written to stdout, so `lux logs`
	// reads like a conversation without an event viewer.
	if u.Kind == "agent_message_chunk" && u.Content.Type == "text" {
		sink.Stdout([]byte(u.Content.Text))
	}
	sink.Event("acp."+u.Kind, json.RawMessage(p.Update))
}

// handleRequest answers agent → client requests. Unattended: permission
// requests are allowed once; anything else is refused.
func (a *ACP) handleRequest(m rpcMsg, sink Sink) {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	switch m.Method {
	case "session/request_permission":
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(m.Params, &p)
		choice := ""
		for _, pref := range []string{"allow_once", "allow_always"} {
			for _, o := range p.Options {
				if o.Kind == pref && choice == "" {
					choice = o.OptionID
				}
			}
		}
		if choice == "" && len(p.Options) > 0 {
			choice = p.Options[0].OptionID
		}
		reply["result"] = map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": choice}}
		sink.Event("acp.permission", map[string]any{"request": json.RawMessage(m.Params), "selected": choice})
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "method not supported by lux: " + m.Method}
	}
	_ = a.lw.send(reply)
}

func (a *ACP) Deliver(in proto.Input) {
	a.mu.Lock()
	if in.Interrupt && a.busy {
		// Put it first, then cancel the running turn; drain sends it when
		// the cancelled prompt returns.
		a.queue = append([]proto.Input{in}, a.queue...)
		session := a.session
		a.mu.Unlock()
		_ = a.notify("session/cancel", map[string]any{"sessionId": session})
		return
	}
	a.queue = append(a.queue, in)
	a.mu.Unlock()
	a.drain()
}

func (a *ACP) Interrupt() error {
	a.mu.Lock()
	session, busy := a.session, a.busy
	a.mu.Unlock()
	if !busy {
		return nil
	}
	return a.notify("session/cancel", map[string]any{"sessionId": session})
}

// Stop cancels the current turn, then closes stdin: an ACP agent exits
// when its client goes away.
func (a *ACP) Stop() error {
	a.mu.Lock()
	a.stopped = true
	session, busy, proc := a.session, a.busy, a.proc
	a.mu.Unlock()
	if busy {
		_ = a.notify("session/cancel", map[string]any{"sessionId": session})
	}
	if proc != nil && proc.Stdin != nil {
		a.lw.mu.Lock()
		_ = proc.Stdin.Close()
		a.lw.w = nil
		a.lw.mu.Unlock()
	}
	return nil
}
