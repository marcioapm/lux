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
	// busTurn: the ACP turn has ended but the Run stays busy until lux
	// knows every steer of it was read, or runs in a loop OpenCode started
	// outside ACP (a steer that reached it as the turn ended, or one sent
	// again after an interrupt), followed on the bus (settle).
	busTurn bool
	// reserved: steers whose prompt_async has not returned; a turn cannot
	// be settled until they have.
	reserved int
	// cancelGen counts the session/cancels sent.
	cancelGen int
	steers    chan proto.Input
	// settleMu serializes settle. suspect: steers seen stored and
	// unanswered with no loop running, by message id, and when first seen.
	settleMu    sync.Mutex
	suspect     map[string]time.Time
	settleEvery time.Duration
	rechecking  bool
	recheckT    *time.Timer
	ctx         context.Context
	// closed: Run is returning; no goroutine starts after it.
	closed bool
	// bg: the adapter's own goroutines, joined when Run returns.
	bg sync.WaitGroup
}

func NewACP() *ACP { return &ACP{ready: make(chan struct{})} }

// NewOpenCode is the ACP adapter for OpenCode.
func NewOpenCode() *ACP {
	a := &ACP{ready: make(chan struct{}), opencode: true, steers: make(chan proto.Input, 256),
		suspect: map[string]time.Time{}, settleEvery: time.Second}
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
	ctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.sink, a.ctx = sink, ctx
	a.mu.Unlock()
	defer a.exited(cancel)
	a.rpc.attach(p.Stdin)

	go pump(p.Stderr, sink.Stderr)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		a.rpc.readLoop(p.Stdout, a.handleRequest, a.handleNotification, sink.Stdout)
	}()

	if a.bus != nil {
		a.bg.Add(1)
		go func() {
			defer a.bg.Done()
			a.bus.follow(ctx, a.onBus, func() { a.settle() })
		}()
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
	a.busy, a.inflight, a.turnEnd = true, 1, nil
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
	a.spawn(func() {
		res, err := wait()
		a.promptDone(res, err, true)
	})
}

// spawn runs f on a goroutine Run joins before it returns; none once Run
// is returning.
func (a *ACP) spawn(f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		f()
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

// exited ends the adapter's own work once the agent has exited: its
// goroutines are stopped and joined, and each input it accepted and the
// agent never read fails.
func (a *ACP) exited(cancel context.CancelFunc) {
	cancel()
	a.mu.Lock()
	a.closed = true
	stopped := a.stopped
	if a.rechecking && a.recheckT.Stop() {
		a.bg.Done()
	}
	a.mu.Unlock()
	a.bg.Wait()
	why := errors.New("the agent exited before it read it")
	if stopped {
		why = errors.New("the Run stopped before the agent read it")
	}
	a.inputs.close(a.sink, why)
}

// runCtx is the Run's context: HTTP calls to OpenCode end with it.
func (a *ACP) runCtx() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// endTurn ends the running turn: acp.turn_end, then idle unless more input
// waits. With OpenCode's server, a turn with steers not known to be read,
// or whose prompt_async has not returned, is settled first (settle).
func (a *ACP) endTurn(data map[string]any) {
	if a.bus != nil {
		a.receipts(a.runCtx())
	}
	// Replies stream in chunks without line breaks: end the turn's text
	// on a line of its own.
	a.sink.EndMessage()
	a.sink.Event("acp.turn_end", data)
	a.mu.Lock()
	if a.bus != nil && (a.reserved > 0 || len(a.inputs.unread("bus")) > 0) {
		a.busTurn = true
		a.mu.Unlock()
		a.settle()
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

// receipts consumes the steers OpenCode's stored messages show read: every
// tracked message at or before an assistant step's parent. It returns the
// ids of the user messages stored, and false if OpenCode did not answer.
func (a *ACP) receipts(ctx context.Context) (map[string]bool, bool) {
	oldest := a.bus.oldest()
	if oldest == "" {
		return nil, true
	}
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	msgs, err := a.bus.messagesSince(ctx, session, oldest)
	if err != nil {
		return nil, false
	}
	stored := map[string]bool{}
	for _, m := range msgs {
		switch {
		case m.Info.Role == "user":
			stored[m.Info.ID] = true
		case m.Info.Role == "assistant" && m.Info.ParentID != "":
			for _, id := range a.bus.answered(m.Info.ParentID) {
				a.inputs.consume(a.sink, id)
			}
		}
	}
	return stored, true
}

// settle decides, once the ACP turn has ended (busTurn), whether OpenCode
// still has work of lux's. It reads OpenCode's stored messages and session
// status rather than trusting bus events, which a dropped stream loses:
//
//   - a steer an assistant step answered is consumed;
//   - while a prompt_async is in flight the Run stays busy (its return
//     settles again), and while OpenCode runs a loop, until session.idle;
//   - a steer not stored yet is still on its way;
//   - a steer stored and unanswered while no loop runs was dropped: by a
//     loop cancelled after it was sent, when seen so on two looks
//     settleEvery apart, or else by a loop that ended just as it was
//     stored, when seen so for 3×settleEvery (a loop it started would have
//     run or answered it by then). It is sent again, so it starts the next
//     loop, or fails if the Run is stopping;
//   - with none of these, the Run's work has ended (busTurnEnded).
//
// While it waits it looks again every settleEvery, so a lost session.idle
// cannot leave the Run busy. Also run when the event stream (re)connects;
// before the ACP turn has ended it only consumes.
func (a *ACP) settle() {
	if a.bus == nil {
		return
	}
	a.settleMu.Lock()
	defer a.settleMu.Unlock()
	ctx := a.runCtx()
	if ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	bt, session, reserved := a.busTurn, a.session, a.reserved
	a.mu.Unlock()
	if !bt {
		a.receipts(ctx)
		return
	}
	if reserved > 0 {
		return
	}
	busy, err := a.bus.sessionBusy(ctx, session)
	// Read after the status: a message stored and unanswered here was
	// stored while no loop ran.
	stored, ok := a.receipts(ctx)
	if (err != nil || !ok) && len(a.inputs.unread("bus")) == 0 {
		// OpenCode does not say, and nothing of lux's is unread: any loop
		// still running reads only what it has read.
		a.busTurnEnded()
		return
	}
	if err != nil || !ok || busy {
		a.mu.Lock()
		if busy {
			clear(a.suspect)
		}
		a.mu.Unlock()
		a.recheck()
		return
	}
	now, waiting := time.Now(), false
	for _, in := range a.inputs.unread("bus") {
		msgID, gen := a.bus.messageOf(in.RequestID)
		if !stored[msgID] {
			waiting = true
			continue
		}
		a.mu.Lock()
		since, seen := a.suspect[msgID]
		if !seen {
			a.suspect[msgID] = now
		}
		wait := 3 * a.settleEvery
		if a.cancelGen > gen {
			wait = a.settleEvery
		}
		a.mu.Unlock()
		if !seen || now.Sub(since) < wait {
			waiting = true
			continue
		}
		a.mu.Lock()
		delete(a.suspect, msgID)
		a.mu.Unlock()
		if a.carry(ctx, session, in) {
			waiting = true
		}
	}
	if waiting {
		a.recheck()
		return
	}
	a.busTurnEnded()
}

// recheck settles again in settleEvery, unless a look is already due.
func (a *ACP) recheck() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rechecking || a.closed || a.ctx == nil || a.ctx.Err() != nil {
		return
	}
	a.rechecking = true
	a.bg.Add(1)
	a.recheckT = time.AfterFunc(a.settleEvery, func() {
		defer a.bg.Done()
		a.mu.Lock()
		a.rechecking = false
		a.mu.Unlock()
		a.settle()
	})
}

// carry sends a steer OpenCode dropped unread (its loop was cancelled)
// again, under the same request id and a new message id, so it starts the
// next loop; the dropped copy stays in the conversation, unanswered. When
// the Run is stopping, or OpenCode refuses it, it fails instead. It reports
// whether it was sent.
func (a *ACP) carry(ctx context.Context, session string, in proto.Input) bool {
	a.bus.untrackRequest(in.RequestID)
	a.mu.Lock()
	stopped, gen := a.stopped, a.cancelGen
	a.mu.Unlock()
	if stopped {
		a.inputs.fail(a.sink, in, errors.New("the Run stopped before the agent read it"))
		return false
	}
	msgID := a.bus.messageID(time.Now())
	a.bus.track(msgID, in.RequestID, gen)
	if err := a.bus.promptAsync(ctx, session, msgID, in.Text); err != nil {
		a.bus.untrack(msgID)
		a.inputs.fail(a.sink, in, fmt.Errorf("sending it again after the turn was cancelled: %w", err))
		return false
	}
	return true
}

// busTurnEnded ends the Run's work after its ACP turn: the loops OpenCode
// ran for steers the turn left behind.
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
	ctx := a.runCtx()
	a.inputs.track(in)
	a.mu.Lock()
	session, busy := a.session, a.busy && !a.busTurn
	a.mu.Unlock()
	if !busy {
		// The turn ended meanwhile: the input starts the next one.
		a.queueInput(in)
		return
	}
	if a.bus != nil && a.bus.waitConnected(ctx, 5*time.Second) {
		msgID := a.bus.messageID(time.Now())
		// Reserved against the running turn: it is not settled until the
		// request returns.
		a.mu.Lock()
		if !a.busy || a.busTurn || a.stopped {
			a.mu.Unlock()
			a.queueInput(in)
			return
		}
		a.reserved++
		a.bus.track(msgID, in.RequestID, a.cancelGen)
		a.mu.Unlock()
		err := a.bus.promptAsync(ctx, session, msgID, in.Text)
		if err == nil {
			a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "bus")
		} else {
			a.bus.untrack(msgID)
		}
		a.mu.Lock()
		a.reserved--
		settle := a.busTurn && a.reserved == 0
		a.mu.Unlock()
		switch {
		case err == nil:
		case !errors.Is(err, errNotSent):
			a.inputs.fail(a.sink, in, err)
		case settle:
			// The turn ended while OpenCode refused it: the next turn.
			a.queueInput(in)
		default:
			a.sink.Event(proto.EvWarning, map[string]any{"message": "opencode: steering over ACP instead: " + err.Error()})
			a.steerACP(session, in)
		}
		if settle {
			a.settle()
		}
		return
	} else if a.bus != nil {
		msg := "opencode: its event stream is not connected; steering over ACP, without a receipt"
		if err := a.bus.err(); err != nil {
			msg += ": " + err.Error()
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": msg})
	}
	a.steerACP(session, in)
}

// steerACP sends a steer as a second session/prompt, which OpenCode joins
// to the running loop; no receipt.
func (a *ACP) steerACP(session string, in proto.Input) {
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
	a.spawn(func() {
		res, err := wait()
		a.promptDone(res, err, false)
	})
}

// queueInput holds input for the next turn.
func (a *ACP) queueInput(in proto.Input) {
	a.inputs.forget(in.RequestID)
	a.mu.Lock()
	a.queue = append(a.queue, in)
	a.mu.Unlock()
	a.drain()
}

// onBus follows OpenCode's bus: an assistant step is every steer at or
// before its parent read; session.idle after the ACP turn has ended
// settles the Run's work.
func (a *ACP) onBus(ev busEvent) {
	a.mu.Lock()
	session, bt := a.session, a.busTurn
	a.mu.Unlock()
	p := ev.Properties
	switch ev.Type {
	case "message.updated":
		if p.Info.Role == "assistant" && p.Info.SessionID == session && p.Info.ParentID != "" {
			for _, id := range a.bus.answered(p.Info.ParentID) {
				a.inputs.consume(a.sink, id)
			}
		}
	case "session.idle":
		if p.SessionID == session && bt {
			a.settle()
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
		// steers the turn left unread start the next one (settle).
		if in.Text != "" {
			a.queue = append([]proto.Input{in}, a.queue...)
		}
		session := a.session
		a.cancelGen++
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
		a.cancelGen++
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
