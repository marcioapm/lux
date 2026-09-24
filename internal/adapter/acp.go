package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
	rpc     rpcConn
	mu      sync.Mutex
	sink    Sink
	session string
	busy    bool
	stopped bool
	queue   []proto.Input
	ready   chan struct{}
	// loading: session/load replays the conversation as updates; they are
	// events, not new output.
	loading bool
}

func NewACP() *ACP { return &ACP{ready: make(chan struct{})} }

func (a *ACP) Command(cfg proto.ShimConfig) ([]string, error) { return command(cfg) }

func (a *ACP) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	a.rpc.attach(p.Stdin)

	go pump(p.Stderr, sink.Stderr)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		a.rpc.readLoop(p.Stdout, a.handleRequest, a.handleNotification, sink.Stdout)
	}()

	err := a.handshake(cfg)
	if err != nil {
		sink.Event(proto.EvWarning, map[string]any{"message": "acp: " + err.Error()})
		_ = p.Signal(syscall.SIGTERM)
	} else {
		close(a.ready)
		a.drain()
	}
	<-readDone
	return err
}

func (a *ACP) handshake(cfg proto.ShimConfig) error {
	res, err := a.rpc.call("initialize", map[string]any{
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
	cwd := workdir(cfg)
	if cfg.Resume && cfg.SessionID != "" && init.AgentCapabilities.LoadSession {
		a.setLoading(true)
		_, err := a.rpc.call("session/load", map[string]any{"sessionId": cfg.SessionID, "cwd": cwd, "mcpServers": []any{}})
		a.setLoading(false)
		if err == nil {
			a.setSession(cfg.SessionID)
			return nil
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": "session/load failed, starting a new session: " + err.Error()})
	}
	res, err = a.rpc.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
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

func (a *ACP) setLoading(v bool) {
	a.mu.Lock()
	a.loading = v
	a.mu.Unlock()
}

func (a *ACP) setSession(id string) {
	a.mu.Lock()
	a.session = id
	a.mu.Unlock()
	a.sink.Session(id)
	a.sink.Activity(true)
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
	session := a.session
	a.mu.Unlock()

	a.sink.Activity(false)
	a.sink.InputAck(in, nil)
	go func() {
		res, err := a.rpc.call("session/prompt", map[string]any{"sessionId": session, "prompt": textInput(in.Text)})
		var pr struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(res, &pr)
		data := withUsage(map[string]any{"stopReason": pr.StopReason}, pr.Usage)
		if err != nil {
			data["error"] = err.Error()
		}
		// Replies stream in chunks without line breaks: end the turn's text
		// on a line of its own.
		a.sink.EndMessage()
		a.sink.Event("acp.turn_end", data)
		a.mu.Lock()
		a.busy = false
		idle := len(a.queue) == 0
		a.mu.Unlock()
		if idle {
			a.sink.Activity(true)
		}
		a.drain()
	}()
}

func (a *ACP) handleNotification(m rpcMsg) {
	if m.Method != "session/update" {
		a.sink.Event("acp."+m.Method, json.RawMessage(m.Params))
		return
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
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
	a.mu.Lock()
	loading := a.loading
	a.mu.Unlock()
	if u.Kind == "agent_message_chunk" && u.Content.Type == "text" && !loading && u.Content.Text != "" {
		a.sink.Stdout([]byte(u.Content.Text))
	}
	// Chunked text (agent_message_chunk, agent_thought_chunk, …) is
	// streamed, so a secret split across chunks is redacted whole.
	if strings.HasSuffix(u.Kind, "_chunk") && u.Content.Type == "text" &&
		streamEvent(a.sink, "acp."+u.Kind, p.SessionID+"\x00", p.Update, [][]string{{"content", "text"}}) {
		return
	}
	a.sink.Event("acp."+u.Kind, json.RawMessage(p.Update))
}

// handleRequest answers agent → client requests. Unattended: permission
// requests are allowed once; anything else is refused.
func (a *ACP) handleRequest(m rpcMsg) {
	if m.Method != "session/request_permission" {
		_ = a.rpc.replyError(m.ID, -32601, "method not supported by lux: "+m.Method)
		return
	}
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
	_ = a.rpc.reply(m.ID, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": choice}})
	a.sink.Event("acp.permission", map[string]any{"request": json.RawMessage(m.Params), "selected": choice})
}

func (a *ACP) Deliver(in proto.Input) {
	a.mu.Lock()
	if in.Interrupt && a.busy {
		// Put it first, then cancel the running turn; drain sends it when
		// the cancelled prompt returns.
		a.queue = append([]proto.Input{in}, a.queue...)
		session := a.session
		a.mu.Unlock()
		_ = a.rpc.notify("session/cancel", map[string]any{"sessionId": session})
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
	return a.rpc.notify("session/cancel", map[string]any{"sessionId": session})
}

// Stop cancels the current turn, then closes stdin: an ACP agent exits
// when its client goes away.
func (a *ACP) Stop() error {
	_ = a.Interrupt()
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	a.rpc.lw.close()
	return nil
}
