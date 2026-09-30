package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// ACP speaks the Agent Client Protocol: JSON-RPC 2.0, newline-delimited, on
// the agent's stdin/stdout. One adapter for every ACP agent.
//
// ACP has no mid-turn message, so input that arrives during a turn is
// queued and sent as the next session/prompt when the turn ends; with
// interrupt it cancels the turn (session/cancel) first.
//
// OpenCode (NewOpenCode) takes input during a turn at the agent's next
// step: lux sends it through OpenCode's HTTP server (opencode.go), which
// says when it was read, or else as a second session/prompt, which
// OpenCode joins to the running loop. Either way the turn ends once, when
// every prompt of it has resolved.
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
	inputs  inputLedger

	// OpenCode only.
	opencode bool
	bus      *opencodeBus
	// inflight: session/prompt calls of the running turn not yet
	// resolved; the turn ends when it drops to 0. turnEnd is the
	// turn-starting prompt's result, which acp.turn_end reports (joined
	// prompts resolve with the same stopReason and usage).
	inflight int
	turnEnd  map[string]any
	// busTurn: a loop OpenCode runs for a steer that reached it just after
	// the ACP turn ended, followed on the bus.
	busTurn bool
	// cancelled: session/cancel was sent during the running turn.
	cancelled bool
	steers    chan proto.Input
}

func NewACP() *ACP { return &ACP{ready: make(chan struct{})} }

// NewOpenCode is the ACP adapter for OpenCode.
func NewOpenCode() *ACP {
	a := &ACP{ready: make(chan struct{}), opencode: true, steers: make(chan proto.Input, 256)}
	go a.steerLoop()
	return a
}

// Command: for OpenCode, `opencode acp` also serves its HTTP API on a
// loopback port (--port), which lux steers through. A command lux did not
// build (no "acp" argument, a --port of its own, or a resume command) is
// run as given, and steers go over ACP only.
func (a *ACP) Command(cfg proto.ShimConfig) ([]string, error) {
	argv, err := command(cfg)
	if err != nil || !a.opencode || (cfg.Resume && len(cfg.ResumeCommand) > 0) ||
		!slices.Contains(argv, "acp") || slices.ContainsFunc(argv, func(s string) bool { return strings.HasPrefix(s, "--port") || strings.HasPrefix(s, "--hostname") }) {
		return argv, err
	}
	port, perr := freeLoopbackPort()
	if perr != nil {
		return argv, nil
	}
	a.bus = newOpencodeBus(port, workdir(cfg))
	return append(slices.Clone(argv), "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"), nil
}

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

	if a.bus != nil {
		busCtx, stopBus := context.WithCancel(ctx)
		defer stopBus()
		go a.bus.follow(busCtx, a.onBus)
	}
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
			LoadSession     bool `json:"loadSession"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &init)
	cwd := workdir(cfg)
	mcp := acpMCPServers(cfg.MCP)
	if len(mcp) > 0 && !init.AgentCapabilities.MCPCapabilities.HTTP {
		// ACP lets a client send only transports the agent advertises.
		a.sink.Event(proto.EvWarning, map[string]any{"message": "the agent does not support HTTP MCP servers: workload.mcpServers are not given to it"})
		mcp = []any{}
	}
	if cfg.Resume && cfg.SessionID != "" && init.AgentCapabilities.LoadSession {
		a.setLoading(true)
		_, err := a.rpc.call("session/load", map[string]any{"sessionId": cfg.SessionID, "cwd": cwd, "mcpServers": mcp})
		a.setLoading(false)
		if err == nil {
			a.setSession(cfg.SessionID)
			return nil
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": "session/load failed, starting a new session: " + err.Error()})
	}
	res, err = a.rpc.call("session/new", map[string]any{"cwd": cwd, "mcpServers": mcp})
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

// acpMCPServers are the servers as ACP HTTP McpServer entries.
func acpMCPServers(servers []proto.MCPServer) []any {
	out := []any{}
	for _, s := range servers {
		headers := []map[string]string{}
		for _, h := range s.Headers {
			headers = append(headers, map[string]string{"name": h.Name, "value": h.Value})
		}
		out = append(out, map[string]any{"type": "http", "name": s.Name, "url": s.URL, "headers": headers})
	}
	return out
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
	a.busy, a.inflight, a.turnEnd, a.cancelled = true, 1, nil, false
	session := a.session
	a.mu.Unlock()

	a.sink.Activity(false)
	wait, err := a.rpc.start("session/prompt", map[string]any{"sessionId": session, "prompt": textInput(in.Text)})
	if err != nil {
		a.inputs.fail(a.sink, in, err)
		a.promptDone(nil, err, true)
		return
	}
	// A prompt that starts a turn is read by its first step; lux has no
	// signal for that on ACP.
	lands := LandsNextTurn
	if a.opencode {
		lands = LandsNextStep
	}
	a.inputs.accept(a.sink, in, Delivery{Lands: lands}, "")
	go func() {
		res, err := wait()
		a.promptDone(res, err, true)
	}()
}

// promptDone is a session/prompt of the running turn resolving; the last
// one ends the turn. first: the prompt that started the turn, whose result
// the turn's acp.turn_end carries.
func (a *ACP) promptDone(res json.RawMessage, err error, first bool) {
	var pr struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(res, &pr)
	data := withUsage(map[string]any{"stopReason": pr.StopReason}, pr.Usage)
	if err != nil {
		data["error"] = err.Error()
	}
	a.mu.Lock()
	a.inflight--
	if first || a.turnEnd == nil {
		a.turnEnd = data
	}
	if a.inflight > 0 {
		a.mu.Unlock()
		return
	}
	data = a.turnEnd
	a.mu.Unlock()
	a.endTurn(data)
}

// endTurn ends the running turn: acp.turn_end, then idle unless more input
// waits.
func (a *ACP) endTurn(data map[string]any) {
	if a.bus != nil && len(a.inputs.unread("bus")) > 0 {
		a.mu.Lock()
		cancelled := a.cancelled
		a.mu.Unlock()
		if cancelled || data["stopReason"] == "cancelled" {
			// A cancelled loop does not read what was steered into it: the
			// steers start the next turn (carryBus, after this turn's end).
			defer a.carryBus()
			a.mu.Lock()
			a.busTurn, a.cancelled = true, false
			a.mu.Unlock()
		} else {
			// A steer that reached OpenCode as its loop ended starts a loop
			// of its own: the Run stays busy until the bus says it ended.
			a.mu.Lock()
			a.busTurn = true
			a.mu.Unlock()
		}
	}
	// Replies stream in chunks without line breaks: end the turn's text
	// on a line of its own.
	a.sink.EndMessage()
	a.sink.Event("acp.turn_end", data)
	a.mu.Lock()
	if a.busTurn {
		a.mu.Unlock()
		return
	}
	a.busy = false
	idle := len(a.queue) == 0
	a.mu.Unlock()
	if idle {
		a.sink.Activity(true)
	}
	a.drain()
}

// carryBus sends the steers a cancelled loop left unread again, in order,
// under the same request ids, so they start the next turn: OpenCode runs a
// loop for them, which the bus follows (busTurn). Each goes under a new
// message id, whose answering step is its consumed; the cancelled loop's
// copy stays in the conversation, unanswered. When the Run is stopping, or
// OpenCode refuses one, it fails instead.
func (a *ACP) carryBus() {
	a.mu.Lock()
	stopped, session := a.stopped, a.session
	a.mu.Unlock()
	carried := false
	for _, in := range a.inputs.unread("bus") {
		a.bus.untrackRequest(in.RequestID)
		if stopped {
			a.inputs.fail(a.sink, in, errors.New("the Run stopped before the agent read it"))
			continue
		}
		msgID := a.bus.messageID(time.Now())
		a.bus.track(msgID, in.RequestID)
		if err := a.bus.promptAsync(session, msgID, in.Text); err != nil {
			a.bus.untrack(msgID)
			a.inputs.fail(a.sink, in, fmt.Errorf("sending it again after the turn was cancelled: %w", err))
			continue
		}
		carried = true
	}
	if !carried {
		a.mu.Lock()
		bt := a.busTurn
		a.mu.Unlock()
		if bt {
			a.busTurnEnded()
		}
	}
}

// busTurnEnded ends a loop OpenCode ran outside any ACP prompt.
func (a *ACP) busTurnEnded() {
	a.mu.Lock()
	a.busTurn, a.busy = false, false
	idle := len(a.queue) == 0
	a.mu.Unlock()
	a.sink.EndMessage()
	a.sink.Event("acp.turn_end", map[string]any{"stopReason": "end_turn", "source": "opencode-bus"})
	if idle {
		a.sink.Activity(true)
	}
	a.drain()
}

// steerLoop delivers steers one at a time, in the order they came.
func (a *ACP) steerLoop() {
	for in := range a.steers {
		a.steer(in)
	}
}

// steer delivers input to OpenCode's running turn, read at its next step:
// through the HTTP server, with a receipt, or else as a second
// session/prompt, which OpenCode joins to the running loop (without one).
func (a *ACP) steer(in proto.Input) {
	a.inputs.track(in)
	a.mu.Lock()
	session, busy := a.session, a.busy && !a.busTurn
	a.mu.Unlock()
	if !busy {
		// The turn ended meanwhile: the input starts the next one.
		a.queueInput(in)
		return
	}
	if a.bus != nil && a.bus.waitConnected(5*time.Second) {
		msgID := a.bus.messageID(time.Now())
		a.bus.track(msgID, in.RequestID)
		err := a.bus.promptAsync(session, msgID, in.Text)
		if err == nil {
			a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "bus")
			return
		}
		a.bus.untrack(msgID)
		if !errors.Is(err, errNotSent) {
			a.inputs.fail(a.sink, in, err)
			return
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": "opencode: steering over ACP instead: " + err.Error()})
	} else if a.bus != nil {
		msg := "opencode: its event stream is not connected; steering over ACP, without a receipt"
		if err := a.bus.err(); err != nil {
			msg += ": " + err.Error()
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": msg})
	}
	a.mu.Lock()
	if !a.busy || a.busTurn || a.stopped {
		a.mu.Unlock()
		a.queueInput(in)
		return
	}
	a.inflight++
	a.mu.Unlock()
	wait, err := a.rpc.start("session/prompt", map[string]any{"sessionId": session, "prompt": textInput(in.Text)})
	if err != nil {
		a.inputs.fail(a.sink, in, err)
		a.promptDone(nil, err, false)
		return
	}
	a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextStep}, "")
	go func() {
		res, err := wait()
		a.promptDone(res, err, false)
	}()
}

// queueInput holds input for the next turn.
func (a *ACP) queueInput(in proto.Input) {
	a.inputs.forget(in.RequestID)
	a.mu.Lock()
	a.queue = append(a.queue, in)
	a.mu.Unlock()
	a.drain()
}

// onBus follows OpenCode's bus: an assistant step answering a steer's
// message is the steer read; a loop OpenCode runs outside any ACP turn (a
// steer that arrived as the turn ended) ends with session.idle.
func (a *ACP) onBus(ev busEvent) {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	p := ev.Properties
	switch ev.Type {
	case "message.updated":
		if p.Info.Role == "assistant" && p.Info.SessionID == session && p.Info.ParentID != "" {
			if id, ok := a.bus.answered(p.Info.ParentID); ok {
				a.inputs.consume(a.sink, id)
			}
		}
	case "session.idle":
		// The loop a late steer started has ended once it has read every
		// steer (an idle before that is the ACP turn's own loop ending).
		a.mu.Lock()
		end := p.SessionID == session && a.busTurn
		cancelled := a.cancelled
		a.mu.Unlock()
		if end && cancelled && len(a.inputs.unread("bus")) > 0 {
			a.mu.Lock()
			a.cancelled = false
			a.mu.Unlock()
			a.carryBus()
			return
		}
		if end && len(a.inputs.unread("bus")) == 0 {
			a.busTurnEnded()
		}
	}
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
		// the cancelled prompt returns. An interrupt alone sends nothing:
		// steers the turn left unread start the next one (carryBus).
		if in.Text != "" {
			a.queue = append([]proto.Input{in}, a.queue...)
		}
		session := a.session
		a.cancelled = true
		a.mu.Unlock()
		err := a.rpc.notify("session/cancel", map[string]any{"sessionId": session})
		if in.Text == "" {
			if err != nil {
				a.inputs.fail(a.sink, in, err)
			} else {
				a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextTurn}, "interrupt")
			}
		}
		return
	}
	if a.opencode && a.busy && !a.busTurn && !a.stopped && !in.Interrupt && in.Text != "" {
		a.mu.Unlock()
		a.steers <- in
		return
	}
	a.queue = append(a.queue, in)
	a.mu.Unlock()
	a.drain()
}

func (a *ACP) Interrupt() error {
	a.mu.Lock()
	session, busy := a.session, a.busy
	if busy {
		a.cancelled = true
	}
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
