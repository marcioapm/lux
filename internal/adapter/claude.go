package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"syscall"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// Claude drives Claude Code in stream-json mode (docs/agent-protocols.md):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//
// A user message is one JSON line on stdin; Claude Code queues messages
// sent mid-turn itself. Interrupt is a control_request on stdin. Stop is
// SIGINT, which ends the turn cleanly; the transcript under ~/.claude is
// what a resume (--resume <session-id>) continues from.
type Claude struct {
	lw        lineWriter
	mu        sync.Mutex
	proc      *Process
	sink      Sink
	queue     []proto.Input
	inTurn    int // user messages sent whose result has not arrived
	sessionID string
}

func NewClaude() *Claude { return &Claude{} }

func (c *Claude) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	base := cfg.Command
	if len(base) == 0 {
		base = []string{"claude"}
	}
	argv := append([]string{}, base...)
	argv = append(argv, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	if cfg.Resume && cfg.SessionID != "" {
		argv = append(argv, "--resume", cfg.SessionID)
	}
	return argv, nil
}

func (c *Claude) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	c.mu.Lock()
	c.proc, c.sink = p, sink
	c.lw.w = p.Stdin
	queued := c.queue
	c.queue = nil
	c.mu.Unlock()

	go pump(p.Stderr, sink.Stderr)
	if !cfg.Resume && cfg.Prompt != "" {
		c.send(proto.Input{RequestID: "prompt", Text: cfg.Prompt})
	}
	for _, in := range queued {
		c.Deliver(in)
	}
	if cfg.Resume && cfg.Prompt == "" && len(queued) == 0 {
		sink.Activity(true)
	}

	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.Type == "" {
			sink.Stdout(append(append([]byte{}, line...), '\n'))
			continue
		}
		if m.SessionID != "" && m.SessionID != c.sessionID {
			c.sessionID = m.SessionID
			sink.Session(m.SessionID)
		}
		switch m.Type {
		case "assistant":
			for _, b := range m.Message.Content {
				if b.Type == "text" && b.Text != "" {
					sink.Stdout([]byte(b.Text + "\n"))
				}
			}
		case "result":
			c.mu.Lock()
			if c.inTurn > 0 {
				c.inTurn--
			}
			idle := c.inTurn == 0
			c.mu.Unlock()
			if idle {
				sink.Activity(true)
			}
		}
		sink.Event("claude."+m.Type, json.RawMessage(append([]byte{}, line...)))
	}
	return nil
}

func (c *Claude) send(in proto.Input) {
	err := c.lw.send(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": in.Text}}},
	})
	c.mu.Lock()
	if err == nil {
		c.inTurn++
	}
	sink := c.sink
	c.mu.Unlock()
	if err == nil {
		sink.Activity(false)
	}
	if in.RequestID != "prompt" {
		sink.InputAck(in.RequestID, err)
	}
}

// Deliver writes the message now: Claude Code queues mid-turn messages
// natively. With interrupt, the running turn is interrupted first.
func (c *Claude) Deliver(in proto.Input) {
	c.mu.Lock()
	if c.proc == nil {
		c.queue = append(c.queue, in)
		c.mu.Unlock()
		return
	}
	busy := c.inTurn > 0
	c.mu.Unlock()
	if in.Interrupt && busy {
		_ = c.Interrupt()
	}
	if in.Text != "" {
		c.send(in)
	} else if in.RequestID != "" {
		c.sink.InputAck(in.RequestID, nil)
	}
}

func (c *Claude) Interrupt() error {
	return c.lw.send(map[string]any{
		"type":       "control_request",
		"request_id": ids.New("int"),
		"request":    map[string]string{"subtype": "interrupt"},
	})
}

// Stop sends SIGINT: Claude Code ends the turn cleanly and records it, so
// a resume continues from a consistent transcript. (SIGTERM would leave the
// turn unfinished.)
func (c *Claude) Stop() error {
	c.mu.Lock()
	p := c.proc
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}
