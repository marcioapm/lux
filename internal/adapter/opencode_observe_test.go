package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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
	return ocObservedOn(t, b.port(), env, nil)
}

// ocObservedOn is ocObserved on the server at 127.0.0.1:port; setup, if
// set, runs on the adapter after Command and before Run.
func ocObservedOn(t *testing.T, port int, env []string, setup func(*ACP)) (*ACP, *agentWire, *fullSink, string) {
	t.Helper()
	a := NewOpenCode()
	a.WorkloadEnv(env)
	cmd := jervasionCommand(port)
	if argv, err := a.Command(proto.ShimConfig{Command: cmd, Workdir: "/workspace"}); err != nil || strings.Join(argv, " ") != strings.Join(cmd, " ") {
		t.Fatalf("argv %q, %v", argv, err)
	}
	if setup != nil {
		setup(a)
	}
	sink := &fullSink{inputSink: &inputSink{}}
	w, first := ocStartedOn(t, a, sink, sink.inputSink)
	return a, w, sink, first
}

// recServer is an HTTP server that records each request as "METHOD path",
// and the requests that carried an Authorization header.
type recServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	reqs, authed []string
}

func newRecServer(t *testing.T, h http.HandlerFunc) *recServer {
	s := &recServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			s.authed = append(s.authed, r.Method+" "+r.URL.Path)
		}
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *recServer) port() int { return s.srv.Listener.Addr().(*net.TCPAddr).Port }

func (s *recServer) snapshot() (reqs, authed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs), slices.Clone(s.authed)
}

// onlyBus fails unless every request in reqs is one of the two an observer
// may send.
func onlyBus(t *testing.T, what string, reqs []string) {
	t.Helper()
	for _, r := range reqs {
		if r != "GET /event" && r != "GET /session/status" {
			t.Fatalf("%s: request %q beyond GET /event and GET /session/status (all: %q)", what, r, reqs)
		}
	}
}

// serveStream answers an event stream: server.connected, then events, then
// nothing until the client leaves.
func serveStream(w http.ResponseWriter, r *http.Request, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
	}
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// serveBusy answers as an OpenCode server whose session is busy: its event
// stream says so, as does any other GET.
func serveBusy(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/event") {
		serveStream(w, r, ocStatus("busy"))
		return
	}
	fmt.Fprintf(w, `{"%s":{"type":"busy"}}`, ocSession)
}

// An observed server that redirects GET /event or GET /session/status, to
// another loopback server or to another path of its own: lux does not
// follow. The target gets no request, no Authorization goes anywhere but
// the two bus endpoints, a redirected stream is retried after a backoff, a
// redirected status read is an error, and nothing the targets say (busy)
// reaches the Run's activity.
func TestOpenCodeObserverDoesNotFollowRedirects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoint  string
		elsewhere bool
	}{
		{"event to another server", "/event", true},
		{"event to another path", "/event", false},
		{"status to another server", "/session/status", true},
		{"status to another path", "/session/status", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elsewhere := newRecServer(t, serveBusy)
			target := "/elsewhere" + tc.endpoint
			if tc.elsewhere {
				target = elsewhere.srv.URL + tc.endpoint
			}
			observed := newRecServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == tc.endpoint:
					http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				case r.URL.Path == "/event":
					serveStream(w, r)
				case r.URL.Path == "/session/status":
					fmt.Fprint(w, `{}`)
				default:
					serveBusy(w, r)
				}
			})
			paused := make(chan time.Duration, 1)
			statusErr := make(chan error, 64)
			_, w, sink, first := ocObservedOn(t, observed.port(), []string{"OPENCODE_SERVER_PASSWORD=" + ocPassword}, func(a *ACP) {
				a.bus.pause = func(_ context.Context, d time.Duration) bool {
					paused <- d
					return false // the test has seen the one attempt it needs
				}
				a.statusRead = func(err error) {
					select {
					case statusErr <- err:
					default:
					}
				}
			})
			if tc.endpoint == "/event" {
				select {
				case <-paused:
				case <-waitTimeout():
					t.Fatal("a redirected GET /event was not retried after a backoff")
				}
			} else {
				for done := false; !done; {
					select {
					case err := <-statusErr:
						if err == nil {
							t.Fatal("a redirected status read succeeded")
						}
						done = !errors.Is(err, context.Canceled)
					case <-waitTimeout():
						t.Fatal("no status read")
					}
				}
			}
			w.resolve(first, ocResult)
			checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
			if reqs, authed := elsewhere.snapshot(); len(reqs) != 0 || len(authed) != 0 {
				t.Fatalf("the redirect's target got %q (authorized: %q)", reqs, authed)
			}
			reqs, authed := observed.snapshot()
			onlyBus(t, "observed", reqs)
			onlyBus(t, "observed, authorized", authed)
			if !slices.Contains(reqs, "GET "+tc.endpoint) {
				t.Fatalf("observed: no GET %s in %q", tc.endpoint, reqs)
			}
			sink.noSecret(t)
		})
	}
}

// A command's own port is its last --port, in either form; a last --port
// with no value, or one that is not a TCP port, means none, whatever came
// before it. Other flags ending in "port" are not it.
func TestOwnPort(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		port int // 0: none
	}{
		{[]string{"w"}, 0},
		{[]string{"w", "--port", "4096"}, 4096},
		{[]string{"w", "--port=4096"}, 4096},
		{[]string{"w", "--port"}, 0},
		{[]string{"w", "--port", "4096", "--port"}, 0},
		{[]string{"w", "--port=4096", "--port"}, 0},
		{[]string{"w", "--port="}, 0},
		{[]string{"w", "--port", "4096", "--port="}, 0},
		{[]string{"w", "--port", "4096", "--port", "x"}, 0},
		{[]string{"w", "--port=4096", "--port=x"}, 0},
		{[]string{"w", "--port", "4096", "--port", ""}, 0},
		{[]string{"w", "--port", "4096", "--port", "5000"}, 5000},
		{[]string{"w", "--port", "4096", "--port=5000"}, 5000},
		{[]string{"w", "--port=4096", "--port", "5000"}, 5000},
		{[]string{"w", "--port=4096", "--port=5000"}, 5000},
		{[]string{"w", "--port", "x", "--port", "5000"}, 5000},
		{[]string{"w", "--port", "0"}, 0},
		{[]string{"w", "--port", "1"}, 1},
		{[]string{"w", "--port", "65535"}, 65535},
		{[]string{"w", "--port", "65536"}, 0},
		{[]string{"w", "--port", "-1"}, 0},
		{[]string{"w", "--port", "4096", "--port", "65536"}, 0},
		{[]string{"w", "--readiness-port", "4097"}, 0},
		{[]string{"w", "--readiness-port=4097"}, 0},
		{[]string{"w", "--port", "4096", "--readiness-port", "4097"}, 4096},
		{[]string{"w", "--portx", "4096"}, 0},
		{[]string{"w", "--port", "--readiness-port", "4097"}, 0},
	} {
		port, ok := ownPort(tc.argv)
		if ok != (tc.port != 0) || (ok && port != tc.port) {
			t.Errorf("ownPort(%q) = %d, %v; want %d", tc.argv, port, ok, tc.port)
		}
	}
}

// A command lux did not build is followed on its own port, and its own
// port only: a trailing --port with no valid value leaves it unobserved
// rather than falling back to an earlier one, so the server there gets no
// request.
func TestOpenCodeObservesOnlyTheLastPort(t *testing.T) {
	earlier := newRecServer(t, serveBusy)
	a := NewOpenCode()
	a.WorkloadEnv([]string{"OPENCODE_SERVER_PASSWORD=" + ocPassword})
	cmd := []string{"wrapper", "--acp", "--port", strconv.Itoa(earlier.port()), "--port"}
	argv, err := a.Command(proto.ShimConfig{Command: cmd, Workdir: "/workspace"})
	if err != nil || strings.Join(argv, " ") != strings.Join(cmd, " ") {
		t.Fatalf("argv %q, %v", argv, err)
	}
	sink := &fullSink{inputSink: &inputSink{}}
	w, first := ocStartedOn(t, a, sink, sink.inputSink)
	w.resolve(first, ocResult)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
	if reqs, _ := earlier.snapshot(); len(reqs) != 0 {
		t.Fatalf("the earlier --port's server got %q", reqs)
	}
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
