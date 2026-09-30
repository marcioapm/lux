package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const ocSession = "ses_f0d8b1671ffexBr8v2rWwmuXTt"

// ocStarted drives an ACP adapter through initialize, session/new and the
// first prompt's session/prompt; it returns that prompt's RPC id.
func ocStarted(t *testing.T, a *ACP) (*agentWire, *inputSink, string) {
	t.Helper()
	w, sink := startWire(t, a, proto.ShimConfig{Prompt: "Run `sleep 20 && echo FIRST`"})
	id, _ := w.next("initialize")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"agentInfo":{"name":"OpenCode","version":"1.18.31"}}}`)
	id, _ = w.next("session/new")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"` + ocSession + `"}}`)
	first, _ := w.next("session/prompt")
	sink.wait(t, "accepted prompt")
	return w, sink, first
}

// The result both prompts of a joined turn get (opencode-acp-prompt-1).
const ocResult = `"result":{"stopReason":"end_turn","usage":{"inputTokens":6,"outputTokens":5,"totalTokens":8380,"cachedReadTokens":8286,"cachedWriteTokens":83},"_meta":{}}`

func checkLines(t *testing.T, sink *inputSink, want ...string) {
	t.Helper()
	sink.waitLast(t, want[len(want)-1])
	time.Sleep(50 * time.Millisecond) // nothing more follows
	if got := sink.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// OpenCode without its HTTP server: a steer during a turn is sent at once
// as a second session/prompt, which OpenCode joins to the running loop.
// Both resolve together at the loop's end: one turn end, one idle.
func TestOpenCodeSteerJoinsTurnOverACP(t *testing.T) {
	a := NewOpenCode()
	w, sink, first := ocStarted(t, a)
	a.Deliver(proto.Input{RequestID: "steer-1", Text: "Before anything else, run `echo STEER`"})
	second, p := w.next("session/prompt")
	if str(p, "sessionId") != ocSession {
		t.Fatalf("params %v", p)
	}
	sink.wait(t, "accepted steer-1")
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	w.send(`{"jsonrpc":"2.0","id":` + second + `,` + ocResult + `}`)
	checkLines(t, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer-1 next_step receipt=false", "turn_end", "idle")
}

// Generic ACP keeps its queue: input during a turn is sent when the turn
// ends, and accepted only once its session/prompt is written.
func TestACPQueuesUntilTurnEnds(t *testing.T) {
	a := NewACP()
	w, sink, first := ocStarted(t, a)
	a.Deliver(proto.Input{RequestID: "later", Text: "x"})
	w.none()
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	second, _ := w.next("session/prompt")
	sink.wait(t, "accepted later next_turn receipt=false")
	w.send(`{"jsonrpc":"2.0","id":` + second + `,` + ocResult + `}`)
	checkLines(t, sink, "idle", "busy", "accepted prompt next_turn receipt=false", "turn_end",
		"busy", "accepted later next_turn receipt=false", "turn_end", "idle")
}

// An ACP prompt whose write fails is failed, never accepted.
func TestACPPromptNotWrittenFails(t *testing.T) {
	a := NewACP()
	w, sink, first := ocStarted(t, a)
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	sink.waitLast(t, "idle")
	a.rpc.lw.close()
	a.Deliver(proto.Input{RequestID: "lost", Text: "x"})
	sink.wait(t, "failed lost: process stdin is closed")
	for _, l := range sink.lines() {
		if strings.HasPrefix(l, "accepted lost") {
			t.Fatalf("%q", sink.lines())
		}
	}
}

// fakeBus is OpenCode's HTTP server as the adapter uses it.
type fakeBus struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events chan string
	posted []map[string]any
	status int
}

func newFakeBus(t *testing.T) *fakeBus {
	b := &fakeBus{events: make(chan string, 64), status: http.StatusNoContent}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-b.events:
				fmt.Fprintf(w, "data: %s\n\n", e)
				w.(http.Flusher).Flush()
			}
		}
	})
	mux.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["session"] = r.PathValue("id")
		body["directory"] = r.Header.Get("x-opencode-directory")
		b.mu.Lock()
		b.posted = append(b.posted, body)
		st := b.status
		b.mu.Unlock()
		w.WriteHeader(st)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBus) port() int { return b.srv.Listener.Addr().(*net.TCPAddr).Port }

// assistant is a model step answering the user message parent, as
// opencode 1.18.31 reports it (opencode-acp-legacy-1).
func assistant(parent string) string {
	return `{"id":"evt_0f29ddf72001","type":"message.updated","properties":{"sessionID":"` + ocSession + `","info":{"id":"msg_0f275509d001A2KKdHyv8k2LwM","role":"assistant","parentID":"` + parent + `","sessionID":"` + ocSession + `","time":{"created":1790774169757}}}}`
}

func ocWithBus(t *testing.T) (*ACP, *fakeBus, *agentWire, *inputSink, string) {
	t.Helper()
	b := newFakeBus(t)
	a := NewOpenCode()
	a.bus = newOpencodeBus(b.port(), "/workspace")
	w, sink, first := ocStarted(t, a)
	for end := time.Now().Add(5 * time.Second); !a.bus.isConnected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("bus never connected")
		}
	}
	return a, b, w, sink, first
}

// With OpenCode's HTTP server: a steer goes through prompt_async under a
// client message id, and is consumed at the first model step answering
// that message.
func TestOpenCodeSteerReceiptFromBus(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "steer-2", Text: "Before anything else, run `echo STEER`"})
	sink.wait(t, "accepted steer-2 next_step receipt=true")
	w.none() // no second ACP prompt
	b.mu.Lock()
	post := b.posted[0]
	b.mu.Unlock()
	msgID, _ := post["messageID"].(string)
	parts, _ := json.Marshal(post["parts"])
	if !strings.HasPrefix(msgID, "msg_") || len(msgID) != 30 || post["session"] != ocSession || post["directory"] != "/workspace" ||
		string(parts) != `[{"text":"Before anything else, run `+"`echo STEER`"+`","type":"text"}]` {
		t.Fatalf("prompt_async %v", post)
	}
	// The running step (the sleep) still answers the first prompt.
	b.events <- assistant("msg_0f274ed8a001VXpumPDla0AnsH")
	b.events <- assistant(msgID)
	b.events <- assistant(msgID)
	sink.wait(t, "consumed steer-2")
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	checkLines(t, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer-2 next_step receipt=true", "consumed steer-2", "turn_end", "idle")
}

// Interrupted with steers unread: OpenCode's cancelled loop never reads
// them, so they are sent again (same request id, new message id) and start
// the next turn, followed on the bus; consumed once, nothing fails.
func TestOpenCodeSteerCarriedPastInterrupt(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	sink.wait(t, "accepted int-1")
	w.send(`{"jsonrpc":"2.0","id":` + first + `,"result":{"stopReason":"cancelled","_meta":{}}}`)
	var again string
	for end := time.Now().Add(5 * time.Second); again == "" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		b.mu.Lock()
		if len(b.posted) == 2 {
			again = b.posted[1]["messageID"].(string)
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	firstID := b.posted[0]["messageID"].(string)
	b.mu.Unlock()
	if again == "" || again == firstID {
		t.Fatalf("not sent again: %q", again)
	}
	w.none()                       // no empty prompt for the interrupt
	b.events <- assistant(firstID) // the cancelled copy: not ours any more
	b.events <- `{"type":"session.status","properties":{"sessionID":"` + ocSession + `","status":{"type":"busy"}}}`
	b.events <- assistant(again)
	b.events <- `{"type":"session.idle","properties":{"sessionID":"` + ocSession + `"}}`
	checkLines(t, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "accepted int-1 next_turn receipt=false", "turn_end",
		"consumed s1", "turn_end", "idle")
}

// Message ids sort after each other, as OpenCode orders messages by id.
func TestOpenCodeMessageIDsAscend(t *testing.T) {
	b := newOpencodeBus(1, "/")
	now := time.UnixMilli(1790776809335)
	prev := ""
	for i := range 5 {
		id := b.messageID(now.Add(time.Duration(i/2) * time.Millisecond))
		if id <= prev || !strings.HasPrefix(id, "msg_0f29d977") {
			t.Fatalf("%s after %s", id, prev)
		}
		prev = id
	}
}

// A steer that reaches OpenCode as its loop ends starts a loop of its own:
// the Run stays busy until the bus says that loop ended.
func TestOpenCodeLateSteerKeepsRunBusy(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	sink.wait(t, "accepted late")
	b.mu.Lock()
	msgID := b.posted[0]["messageID"].(string)
	b.mu.Unlock()
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	sink.wait(t, "turn_end")
	time.Sleep(50 * time.Millisecond)
	if l := sink.lines(); l[len(l)-1] == "idle" {
		t.Fatalf("idle with a steer unread: %q", l)
	}
	b.events <- `{"type":"session.status","properties":{"sessionID":"` + ocSession + `","status":{"type":"busy"}}}`
	b.events <- assistant(msgID)
	b.events <- `{"type":"session.idle","properties":{"sessionID":"` + ocSession + `"}}`
	checkLines(t, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "turn_end", "consumed late", "turn_end", "idle")
}

// prompt_async refusing the steer (it never reached OpenCode): it goes as
// a second ACP prompt instead.
func TestOpenCodeFallsBackToACP(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	b.mu.Lock()
	b.status = http.StatusBadRequest
	b.mu.Unlock()
	a.Deliver(proto.Input{RequestID: "s", Text: "x"})
	second, _ := w.next("session/prompt")
	sink.wait(t, "accepted s next_step receipt=false")
	w.send(`{"jsonrpc":"2.0","id":` + first + `,` + ocResult + `}`)
	w.send(`{"jsonrpc":"2.0","id":` + second + `,` + ocResult + `}`)
	sink.waitLast(t, "idle")
}

// A /event request OpenCode takes while starting and never answers
// (1.18.31 does this) is retried, so the stream still connects.
func TestOpenCodeBusRetriesUnansweredStream(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-r.Context().Done() // no headers, ever
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	b := newOpencodeBus(srv.Listener.Addr().(*net.TCPAddr).Port, "/")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.follow(ctx, func(busEvent) {})
	if !b.waitConnected(8 * time.Second) {
		t.Fatalf("never connected: %v", b.err())
	}
}

// The command gets a loopback --port only when lux builds it.
func TestOpenCodeCommand(t *testing.T) {
	a := NewOpenCode()
	argv, _ := a.Command(proto.ShimConfig{Command: []string{"opencode", "acp"}, Workdir: "/w"})
	if len(argv) != 6 || argv[2] != "--port" || argv[4] != "--hostname" || argv[5] != "127.0.0.1" || a.bus == nil || a.bus.dir != "/w" {
		t.Fatalf("argv %q", argv)
	}
	for _, cmd := range [][]string{{"opencode", "acp", "--port", "5000"}, {"my-agent"}} {
		b := NewOpenCode()
		if argv, _ := b.Command(proto.ShimConfig{Command: cmd}); strings.Join(argv, " ") != strings.Join(cmd, " ") || b.bus != nil {
			t.Fatalf("%q -> %q", cmd, argv)
		}
	}
	if argv, _ := NewACP().Command(proto.ShimConfig{Command: []string{"opencode", "acp"}}); len(argv) != 2 {
		t.Fatalf("generic acp %q", argv)
	}
}
