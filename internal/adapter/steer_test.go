package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// inputSink records what an adapter reports about inputs, activity and
// turn ends, in order.
type inputSink struct {
	mu  sync.Mutex
	log []string
}

func (s *inputSink) add(l string) {
	s.mu.Lock()
	s.log = append(s.log, l)
	s.mu.Unlock()
}
func (s *inputSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.log...)
}
func (s *inputSink) Stdout([]byte)                                   {}
func (s *inputSink) EndMessage()                                     {}
func (s *inputSink) Stderr([]byte)                                   {}
func (s *inputSink) Session(string)                                  {}
func (s *inputSink) Stream(string, string, string, func(string) any) {}
func (s *inputSink) Event(typ string, _ any) {
	if strings.HasSuffix(typ, "turn_end") {
		s.add("turn_end")
	}
}
func (s *inputSink) Activity(idle bool) {
	if idle {
		s.add("idle")
	} else {
		s.add("busy")
	}
}
func (s *inputSink) InputAccepted(in proto.Input, d Delivery) {
	s.add(fmt.Sprintf("accepted %s %s receipt=%v", in.RequestID, d.Lands, d.Receipt))
}
func (s *inputSink) InputConsumed(id string) { s.add("consumed " + id) }
func (s *inputSink) InputFailed(in proto.Input, err error) {
	s.add(fmt.Sprintf("failed %s: %v", in.RequestID, err))
}

// wait polls until the sink has a line with want, or fails.
func (s *inputSink) wait(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		for _, l := range s.lines() {
			if strings.HasPrefix(l, want) {
				return
			}
		}
	}
	t.Fatalf("no %q in %q", want, s.lines())
}

// waitLast polls until the sink's last line is want, after a turn end.
func (s *inputSink) waitLast(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		l := s.lines()
		if len(l) > 1 && l[len(l)-1] == want && slices.Contains(l, "turn_end") {
			return
		}
	}
	t.Fatalf("last line is not %q: %q", want, s.lines())
}

// agentWire is the agent's end of an adapter's stdio: the test reads what
// the adapter sends and writes what the agent answers.
type agentWire struct {
	t    *testing.T
	in   *bufio.Scanner
	out  io.Writer
	sent chan map[string]json.RawMessage
}

func startWire(t *testing.T, ad Adapter, cfg proto.ShimConfig) (*agentWire, *inputSink) {
	t.Helper()
	toAgent, fromAdapter := io.Pipe()
	fromAgent, toAdapter := io.Pipe()
	sink := &inputSink{}
	w := &agentWire{t: t, in: bufio.NewScanner(toAgent), out: toAdapter, sent: make(chan map[string]json.RawMessage, 64)}
	go func() {
		for w.in.Scan() {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(w.in.Bytes(), &m)
			w.sent <- m
		}
	}()
	p := &Process{Cmd: &exec.Cmd{}, Stdin: fromAdapter, Stdout: fromAgent, Stderr: io.NopCloser(strings.NewReader(""))}
	go func() { _ = ad.Run(context.Background(), p, cfg, sink) }()
	t.Cleanup(func() { toAdapter.Close(); toAgent.Close() })
	return w, sink
}

// next returns the next message the adapter sent; method must match.
func (w *agentWire) next(method string) (id string, params map[string]json.RawMessage) {
	w.t.Helper()
	select {
	case m := <-w.sent:
		var got string
		_ = json.Unmarshal(m["method"], &got)
		if got != method {
			w.t.Fatalf("adapter sent %s, want %s", got, method)
		}
		_ = json.Unmarshal(m["params"], &params)
		return string(m["id"]), params
	case <-time.After(5 * time.Second):
		w.t.Fatalf("adapter sent no %s", method)
	}
	return "", nil
}

func (w *agentWire) none() {
	w.t.Helper()
	select {
	case m := <-w.sent:
		w.t.Fatalf("adapter sent %s", m["method"])
	case <-time.After(100 * time.Millisecond):
	}
}

func (w *agentWire) send(line string) {
	w.t.Helper()
	if _, err := io.WriteString(w.out, line+"\n"); err != nil {
		w.t.Fatal(err)
	}
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

const (
	cxThread = "01a0f274-d88c-71b0-942c-1bfdf6e19884"
	cxTurn   = "01a0f274-d9bc-7a52-a1a0-0854785aabb4"
)

// codexStarted drives a Codex adapter through initialize (reporting
// userAgent), thread/start and the prompt's turn/start, as recorded from
// codex 0.155.1 (codex-appserver-1).
func codexStarted(t *testing.T, userAgent string) (*Codex, *agentWire, *inputSink) {
	t.Helper()
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "Run `sleep 20 && echo FIRST`"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"` + userAgent + ` (Mac OS 26.5.1; arm64) unknown (lux; 1)","codexHome":"/h","platformFamily":"unix","platformOs":"macos"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `","status":{"type":"idle"}}}}`)
	id, p := w.next("turn/start")
	if str(p, "clientUserMessageId") != "prompt" {
		t.Fatalf("turn/start without the prompt's id: %s", p["clientUserMessageId"])
	}
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","items":[],"itemsView":"notLoaded","status":"inProgress","error":null}}}`)
	w.send(`{"method":"turn/started","params":{"threadId":"` + cxThread + `","turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f274-da9b","clientId":"prompt","content":[{"type":"text","text":"Run","text_elements":[]}]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "consumed prompt")
	return c, w, sink
}

func cxCompleted(status string) string {
	return `{"method":"turn/completed","params":{"threadId":"` + cxThread + `","turn":{"id":"` + cxTurn + `","status":"` + status + `"}}}`
}

// A steer on Codex 0.155: accepted when turn/steer returns, consumed when
// its userMessage item (clientId = the request id) starts, one turn.
func TestCodexSteerReceipt(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "steer-352FCA", Text: "Before anything else, run `echo STEER`"})
	id, p := w.next("turn/steer")
	if str(p, "expectedTurnId") != cxTurn || str(p, "clientUserMessageId") != "steer-352FCA" {
		t.Fatalf("turn/steer params %v", p)
	}
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted steer-352FCA")
	w.send(`{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"call_1","command":"/bin/zsh -lc 'sleep 20 && echo FIRST'","status":"completed","aggregatedOutput":"FIRST\n"},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[{"type":"text","text":"Before anything else","text_elements":[]}]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `","startedAtMs":1790774170127},"emittedAtMs":1790774170128}`)
	// The item repeats as completed; one consumed only.
	w.send(`{"method":"item/completed","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(cxCompleted("completed"))
	sink.waitLast(t, "idle")
	want := []string{"busy", "accepted prompt next_step receipt=true", "consumed prompt",
		"accepted steer-352FCA next_step receipt=true", "consumed steer-352FCA", "turn_end", "idle"}
	if got := sink.lines()[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Codex 0.144 emits the userMessage item on admission, mid-tool: there it
// is no receipt, so none is promised and none is reported.
func TestCodexOldVersionHasNoReceipt(t *testing.T) {
	for ua, want := range map[string]bool{"lux/0.144.1": false, "lux/0.155.0": true, "lux/0.159.2": true, "lux/1.0.0": true, "weird": false} {
		if codexReceipt(ua+" (x)") != want {
			t.Errorf("%s: receipt %v", ua, !want)
		}
	}
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "p"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.144.1 (Mac OS 26.5.1; arm64) unknown (lux; 1)"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `"}}}`)
	id, _ = w.next("turn/start")
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	sink.wait(t, "accepted prompt")
	c.Deliver(proto.Input{RequestID: "steer-4B54AB", Text: "s"})
	id, _ = w.next("turn/steer")
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f28d-960d","clientId":"steer-4B54AB","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(cxCompleted("completed"))
	sink.waitLast(t, "idle")
	for _, l := range sink.lines() {
		if strings.HasPrefix(l, "consumed") || strings.HasPrefix(l, "failed") || strings.Contains(l, "receipt=true") {
			t.Fatalf("0.144: %q", sink.lines())
		}
	}
}

// Input that arrives while turn/start is in flight is steered into that
// turn as soon as its id is known, not held for the next turn.
func TestCodexInputWhileStartingIsSteered(t *testing.T) {
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "p"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.155.1"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `"}}}`)
	startID, _ := w.next("turn/start")
	c.Deliver(proto.Input{RequestID: "early", Text: "e"})
	w.none()
	w.send(`{"id":` + startID + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	id, p := w.next("turn/steer")
	if str(p, "expectedTurnId") != cxTurn || str(p, "clientUserMessageId") != "early" {
		t.Fatalf("turn/steer params %v", p)
	}
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted early next_step")
}

// Only a steer refused because its turn is gone starts a turn of its own;
// any other refusal is reported as the input's failure.
func TestCodexSteerErrors(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "bad", Text: "x"})
	id, _ := w.next("turn/steer")
	w.send(`{"id":` + id + `,"error":{"code":-32600,"message":"cannot steer a review turn"}}`)
	sink.wait(t, "failed bad: turn/steer: cannot steer a review turn")
	w.none()

	for _, msg := range []string{"no active turn to steer", "expected active turn id `" + cxTurn + "` but found `01a0f2bb-dd3b`"} {
		c.Deliver(proto.Input{RequestID: "late", Text: "y"})
		id, _ = w.next("turn/steer")
		w.send(`{"id":` + id + `,"error":{"code":-32600,"message":"` + msg + `"}}`)
		w.none() // queued: the turn in progress (for the adapter) runs on
		w.send(cxCompleted("completed"))
		id, p := w.next("turn/start")
		if str(p, "clientUserMessageId") != "late" {
			t.Fatalf("turn/start params %v", p)
		}
		w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
		sink.wait(t, "accepted late")
		c.inputs.forget("late")
	}
	for _, l := range sink.lines() {
		if strings.HasPrefix(l, "failed late") {
			t.Fatalf("%q", sink.lines())
		}
	}
}

// A steer accepted into a turn that is then interrupted is never read
// (Codex drops it): it fails rather than waiting for a receipt forever.
func TestCodexSteerDroppedByInterrupt(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "s1", Text: "y"})
	id, _ := w.next("turn/steer")
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted s1")
	w.send(cxCompleted("interrupted"))
	sink.wait(t, "failed s1: the turn ended before the agent read it")
}
