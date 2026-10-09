package adapter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const ocPassword = "s3cr3t-opencode-pw"

// fullSink is an inputSink that also keeps every event, with its data,
// and everything written to stdout and stderr, to search for a secret.
type fullSink struct {
	*inputSink
	mu       sync.Mutex
	all      []string
	warnings int
}

func (s *fullSink) keep(v string) {
	s.mu.Lock()
	s.all = append(s.all, v)
	s.mu.Unlock()
}

func (s *fullSink) Event(typ string, v any) {
	b, _ := json.Marshal(v)
	s.keep(typ + " " + string(b))
	if typ == proto.EvWarning {
		s.mu.Lock()
		s.warnings++
		s.mu.Unlock()
	}
	s.inputSink.Event(typ, v)
}
func (s *fullSink) Stdout(p []byte) { s.keep(string(p)) }
func (s *fullSink) Stderr(p []byte) { s.keep(string(p)) }

func (s *fullSink) warned() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.warnings
}

// noSecret fails if the password is in anything the adapter reported.
func (s *fullSink) noSecret(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range append(s.all, s.inputSink.lines()...) {
		if strings.Contains(l, ocPassword) {
			t.Fatalf("the password was reported: %q", l)
		}
	}
}

// jervasionCommand is a wrapper's argv in the shape of Jervasion's
// entrypoint: no bare "acp", a --port of its own the wrapper's opencode
// serves on, and another port that is not OpenCode's.
func jervasionCommand(port int) []string {
	return []string{"jervasion-agent-entrypoint", "--acp", "--port", strconv.Itoa(port), "--readiness-port", "4097", "--policy-oid", "0123abcd"}
}

// ocObserved starts an OpenCode adapter on a Jervasion-shaped command whose
// server is b, with env as the workload's environment, through the first
// prompt.
func ocObserved(t *testing.T, b *fakeBus, env []string) (*ACP, *agentWire, *fullSink, string) {
	t.Helper()
	a := NewOpenCode()
	a.WorkloadEnv(env)
	cmd := jervasionCommand(b.port())
	if argv, err := a.Command(proto.ShimConfig{Command: cmd, Workdir: "/workspace"}); err != nil || strings.Join(argv, " ") != strings.Join(cmd, " ") {
		t.Fatalf("argv %q, %v", argv, err)
	}
	sink := &fullSink{inputSink: &inputSink{}}
	w, first := ocStartedOn(t, a, sink, sink.inputSink)
	return a, w, sink, first
}

// A command lux did not build, with OpenCode's server on its own --port
// behind Basic auth: lux follows that server's bus, authenticated, for the
// Run's activity. A loop a client starts over HTTP shows the Run busy, then
// idle; lux's steers still go over ACP, without receipts, and lux sends
// nothing to the server but its bus and status reads. The password is in
// nothing lux reports.
func TestOpenCodeObservesServerItDidNotStart(t *testing.T) {
	b := newFakeBus(t)
	b.password = ocPassword
	b.setLoop(true) // the first prompt's loop
	a, w, sink, first := ocObserved(t, b, []string{"PATH=/bin", "OPENCODE_SERVER_PASSWORD=" + ocPassword})
	waitGen(t, a, 1)
	waitHandled(t, a)

	a.Deliver(proto.Input{RequestID: "steer", Text: "also check the tests"})
	second, p := w.next("session/prompt")
	if str(p, "sessionId") != ocSession {
		t.Fatalf("params %v", p)
	}
	sink.wait(t, "accepted steer next_step receipt=false")
	b.setLoop(false)
	w.resolve(first, ocResult)
	w.resolve(second, ocResult)
	sink.waitLast(t, "idle")

	// A review a client drives over OpenCode's HTTP API.
	b.setLoop(true)
	b.events <- ocStatus("busy")
	sink.waitLast(t, "busy")
	b.events <- b.answer("msg_0f274ed8a001VXpumPDla0AnsH")
	b.setLoop(false)
	gen := busGen(a)
	b.events <- ocStatus("idle")
	b.events <- ocIdle
	waitGen(t, a, gen+2)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer next_step receipt=false", "turn_end", "idle", "busy", "idle")

	b.mu.Lock()
	authed, refused, reqs := b.authed, b.refused, fmt.Sprint(b.requests)
	b.mu.Unlock()
	if authed == 0 || refused != 0 {
		t.Fatalf("Basic auth: %d requests authenticated, %d refused", authed, refused)
	}
	if b.count("POST /session/"+ocSession+"/prompt_async") != 0 || b.count("GET /session/"+ocSession+"/message") != 0 {
		t.Fatalf("lux used the observed server beyond its bus: %s", reqs)
	}
	sink.noSecret(t)
}

// The server refuses lux's password: one warning, the bus is not asked
// again, and the Run's activity is lux's own turns', with no busy from
// OpenCode.
func TestOpenCodeObservedWrongPasswordWarnsOnce(t *testing.T) {
	b := newFakeBus(t)
	b.password = "the-right-one"
	b.setLoop(true)
	_, w, sink, first := ocObserved(t, b, []string{"OPENCODE_SERVER_PASSWORD=" + ocPassword})
	for end := time.Now().Add(5 * time.Second); sink.warned() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no warning")
		}
	}
	w.resolve(first, ocResult)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
	if n := sink.warned(); n != 1 {
		t.Fatalf("%d warnings", n)
	}
	b.mu.Lock()
	refused, authed := b.refused, b.authed
	b.mu.Unlock()
	if refused != 1 || authed != 0 || b.count("GET /session/status") != 0 {
		t.Fatalf("after a refusal: %d refused, %d authenticated, %d status reads", refused, authed, b.count("GET /session/status"))
	}
	sink.noSecret(t)
}
