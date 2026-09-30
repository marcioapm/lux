package adapter

import (
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// Recorded from claude 2.1.280 (claude-line-uuid-2, -final-2,
// priority-now-1), with lux's uuids in place of the spike's.
func clLifecycle(uuid, state string) string {
	return `{"type":"command_lifecycle","command_uuid":"` + uuid + `","state":"` + state + `","uuid":"2c736942-063c-46df-9801-6f046829518c","session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0"}`
}

const (
	clInit       = `{"type":"system","subtype":"init","cwd":"/workspace","session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0","capabilities":["interrupt_receipt_v1","interrupt_cancel_queued_v1","msg_lifecycle_v1","mcp_read_resource_v1","mcp_tool_ui_meta_v1"],"claude_code_version":"2.1.280"}`
	clInitNoLife = `{"type":"system","subtype":"init","cwd":"/workspace","session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0","capabilities":["interrupt_receipt_v1"]}`
	clToolUse    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"sleep 20 && echo FIRST"}}]},"session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0"}`
	clToolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"FIRST"}]},"session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0"}`
	clResult     = `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"stop_reason":"end_turn","terminal_reason":"completed","result":"DONE","queued_turn_count":0,"usage":{"input_tokens":32,"output_tokens":537},"session_id":"920ddc42-f98d-497e-af68-cfe9ae3997b0"}`
)

// claudeStarted runs a Claude adapter whose first line is the prompt.
func claudeStarted(t *testing.T) (*Claude, *agentWire, *inputSink, string) {
	t.Helper()
	c := NewClaude()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "Run `sleep 20 && echo FIRST`"})
	var line map[string]any
	select {
	case m := <-w.sent:
		for k, v := range m {
			if line == nil {
				line = map[string]any{}
			}
			line[k] = string(v)
		}
	case <-waitTimeout():
		t.Fatal("no prompt line")
	}
	uuid := strings.Trim(line["uuid"].(string), `"`)
	if uuid != claudeUUID("prompt") || line["priority"] != nil {
		t.Fatalf("prompt line %v", line)
	}
	return c, w, sink, uuid
}

// userLine returns the next user line's uuid, and fails on a priority.
func userLine(t *testing.T, w *agentWire) string {
	t.Helper()
	select {
	case m := <-w.sent:
		if _, ok := m["priority"]; ok || str(m, "type") != "user" {
			t.Fatalf("line %v", m)
		}
		return str(m, "uuid")
	case <-waitTimeout():
		t.Fatal("no user line")
	}
	return ""
}

func TestClaudeUUID(t *testing.T) {
	u := claudeUUID("req-1")
	if u != claudeUUID("req-1") || u == claudeUUID("req-2") || len(u) != 36 || u[14] != '5' || !strings.ContainsAny(u[19:20], "89ab") {
		t.Fatalf("uuid %s", u)
	}
}

// A steer folded into the running turn at the tool boundary: queued is
// accepted, started is consumed, and the Run goes idle after the one
// result. On main the adapter counted a line per result, and never went
// idle here.
func TestClaudeFoldedSteerGoesIdle(t *testing.T) {
	c, w, sink, prompt := claudeStarted(t)
	w.send(clLifecycle(prompt, "queued"))
	w.send(clLifecycle(prompt, "started"))
	w.send(clInit)
	w.send(clToolUse)
	c.Deliver(proto.Input{RequestID: "steer-1", Text: "Before anything else, run `echo STEER`"})
	steer := userLine(t, w)
	if steer != claudeUUID("steer-1") {
		t.Fatalf("steer uuid %s", steer)
	}
	w.send(clLifecycle(steer, "queued"))
	sink.wait(t, "accepted steer-1 next_step receipt=true")
	w.send(clToolResult)
	w.send(clLifecycle(steer, "started"))
	w.send(clLifecycle(steer, "completed"))
	w.send(clResult)
	w.send(clLifecycle(prompt, "completed"))
	checkLines(t, sink, "busy", "accepted prompt next_step receipt=true", "consumed prompt",
		"busy", "accepted steer-1 next_step receipt=true", "consumed steer-1", "turn_end", "idle")
}

// When the model's step ends the turn without a tool call, the queued
// line starts the next turn: idle only after its result.
func TestClaudeSteerAfterFinalStepIsNextTurn(t *testing.T) {
	c, w, sink, prompt := claudeStarted(t)
	w.send(clLifecycle(prompt, "queued"))
	w.send(clLifecycle(prompt, "started"))
	w.send(clInit)
	c.Deliver(proto.Input{RequestID: "s", Text: "Also run `echo STEER`"})
	steer := userLine(t, w)
	w.send(clLifecycle(steer, "queued"))
	w.send(strings.Replace(clResult, `"num_turns":4`, `"num_turns":1`, 1))
	w.send(clLifecycle(prompt, "completed"))
	w.send(clLifecycle(steer, "started"))
	w.send(clInit)
	w.send(strings.Replace(clResult, `"num_turns":4`, `"num_turns":2`, 1))
	w.send(clLifecycle(steer, "completed"))
	checkLines(t, sink, "busy", "accepted prompt next_step receipt=true", "consumed prompt",
		"busy", "accepted s next_step receipt=true", "turn_end", "consumed s", "turn_end", "idle")
}

// A line Claude Code discards before reading it fails; one it had already
// read (consumed) stays consumed when its turn is then cancelled.
func TestClaudeCancelledLineFails(t *testing.T) {
	c, w, sink, prompt := claudeStarted(t)
	w.send(clLifecycle(prompt, "queued"))
	w.send(clLifecycle(prompt, "started"))
	c.Deliver(proto.Input{RequestID: "s", Text: "x"})
	steer := userLine(t, w)
	w.send(clLifecycle(steer, "queued"))
	w.send(strings.Replace(clResult, `"completed"`, `"aborted_streaming"`, 1))
	w.send(clLifecycle(prompt, "cancelled"))
	w.send(clLifecycle(steer, "discarded"))
	checkLines(t, sink, "busy", "accepted prompt next_step receipt=true", "consumed prompt",
		"busy", "accepted s next_step receipt=true", "turn_end", "failed s: claude: the message was discarded", "idle")
}

// A Claude Code without msg_lifecycle_v1: accepted as written, no
// receipt, busy from each line to a result.
func TestClaudeWithoutLifecycle(t *testing.T) {
	_, w, sink, _ := claudeStarted(t)
	w.send(clInitNoLife)
	sink.wait(t, "accepted prompt next_step receipt=false")
	w.send(clResult)
	checkLines(t, sink, "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
}
