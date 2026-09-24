package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/marcioapm/lux/internal/proto"
)

// Codex drives `codex app-server`: JSON-RPC 2.0, newline-delimited, on
// stdio (docs/agent-protocols.md). A thread is the conversation (persisted
// as a rollout under ~/.codex); a turn is one exchange. Mid-turn input is
// native (turn/steer); interrupt is turn/interrupt; a resume on another
// host is thread/resume with the stored thread id.
type Codex struct {
	rpc    rpcConn
	mu     sync.Mutex
	sink   Sink
	thread string
	turn   string // in-progress turn id, "" when idle
	// usage is the last thread/tokenUsage/updated of the turn in progress,
	// as Codex sent it, and usageTurn the turnId it named: codex.turn_end
	// carries it. Cleared when a turn starts, so it never leaks into the next.
	usage     json.RawMessage
	usageTurn string
	queue     []proto.Input
	ready     bool
	stopped   bool
}

func NewCodex() *Codex { return &Codex{} }

// CredentialFiles: Codex reads its key from ~/.codex/auth.json (what
// `codex login --with-api-key` writes), not from OPENAI_API_KEY in the
// environment. An OPENAI_API_KEY secret becomes that file.
func (c *Codex) CredentialFiles(secrets map[string]string, home string) map[string][]byte {
	key, ok := secrets["OPENAI_API_KEY"]
	if !ok {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": key})
	return map[string][]byte{filepath.Join(home, ".codex", "auth.json"): b}
}

// Command runs the spec's command (default "codex") with app-server.
func (c *Codex) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	argv, err := command(cfg)
	if err != nil {
		return nil, err
	}
	return append(slices.Clone(argv), "app-server"), nil
}

func (c *Codex) call(method string, params any) (json.RawMessage, error) {
	return c.rpc.call(method, params)
}

func (c *Codex) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	c.mu.Lock()
	c.sink = sink
	c.mu.Unlock()
	c.rpc.attach(p.Stdin)
	go pump(p.Stderr, sink.Stderr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.rpc.readLoop(p.Stdout, c.handleRequest, func(m rpcMsg) { c.handleNotification(m, sink) }, sink.Stdout)
	}()

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
	return nil
}

func (c *Codex) handshake(cfg proto.ShimConfig, sink Sink) error {
	if _, err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "lux", "version": "1"}}); err != nil {
		return err
	}
	cwd := workdir(cfg)
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
	c.usage, c.usageTurn = nil, ""
	thread := c.thread
	sink := c.sink
	c.mu.Unlock()
	sink.Activity(false)
	go func() {
		res, err := c.call("turn/start", map[string]any{"threadId": thread, "input": textInput(in.Text)})
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
		sink.InputAck(in, err)
		if err != nil {
			sink.Activity(true)
			c.drain()
		}
	}()
}

// handleRequest answers server requests (approvals): unattended, so
// approve; the sandbox is the container.
func (c *Codex) handleRequest(m rpcMsg) {
	_ = c.rpc.reply(m.ID, map[string]string{"decision": "approved"})
	c.sink.Event("codex.request."+m.Method, json.RawMessage(m.Params))
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
		// A turn we did not know of (not the one turn/start just named):
		// usage so far belongs to an earlier one.
		if p.Turn.ID != c.turn {
			c.usage, c.usageTurn = nil, ""
		}
		c.turn = p.Turn.ID
		c.mu.Unlock()
	case "thread/tokenUsage/updated":
		var p struct {
			TurnID     string          `json:"turnId"`
			TokenUsage json.RawMessage `json:"tokenUsage"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		// Only the turn in progress's; while it is "starting" its id is
		// not known yet, so take it.
		if c.turn != "" && (p.TurnID == "" || c.turn == "starting" || p.TurnID == c.turn) {
			c.usage, c.usageTurn = p.TokenUsage, p.TurnID
		}
		c.mu.Unlock()
	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		c.turn = ""
		usage := c.usage
		if c.usageTurn != "" && p.Turn.ID != "" && c.usageTurn != p.Turn.ID {
			usage = nil
		}
		c.usage, c.usageTurn = nil, ""
		idle := len(c.queue) == 0
		c.mu.Unlock()
		// The turn's end, with the agent's usage as it reported it (the
		// counterpart of acp.turn_end and claude.turn_end).
		// Sent before idle and before the next turn starts.
		sink.Event("codex.turn_end", withUsage(map[string]any{"status": p.Turn.Status}, usage))
		if idle {
			sink.Activity(true)
		}
		defer c.drain() // after the turn/completed event below
	case "item/completed":
		var p struct {
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Item.Type == "agentMessage" && p.Item.Text != "" {
			sink.Stdout([]byte(p.Item.Text))
			sink.EndMessage()
		}
	}
	// Streamed text (item/agentMessage/delta, item/reasoning/textDelta,
	// command output, …), so a secret split across deltas is redacted whole.
	if strings.HasSuffix(strings.ToLower(m.Method), "delta") &&
		streamEvent(sink, "codex."+m.Method, "", m.Params, [][]string{{"delta"}}) {
		return
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
			"input": textInput(in.Text)})
		if err != nil {
			// The turn ended meanwhile: send it as the next turn.
			c.mu.Lock()
			c.queue = append(c.queue, in)
			c.mu.Unlock()
			c.drain()
			return
		}
		c.sink.InputAck(in, nil)
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
	c.mu.Unlock()
	_ = c.Interrupt()
	c.rpc.lw.close()
	return nil
}
