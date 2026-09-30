package adapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// opencodeBus is OpenCode's own HTTP server, run by the same process as
// its ACP (`opencode acp --port <p>`, on 127.0.0.1): lux sends steers
// through it and reads its event bus to learn when the agent read them.
//
// ACP stdio says nothing when a prompt sent during a turn is read. The
// legacy prompt_async endpoint takes a client message id, stores the
// message under it, and joins the running agent loop (never the v2
// /api/session/{id}/prompt: mixed with ACP it starts a second, concurrent
// loop). Each model step is an assistant message.updated whose parentID is
// the user message it answers, so the first one naming the steer's id is
// the step that read it.
type opencodeBus struct {
	port int
	dir  string
	hc   *http.Client

	mu        sync.Mutex
	connected bool
	// expect maps a steer's message id to its lux request id.
	expect map[string]string
	lastMs int64
	seq    int64
}

// freeLoopbackPort is a port nothing listens on at 127.0.0.1 now.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func newOpencodeBus(port int, dir string) *opencodeBus {
	return &opencodeBus{port: port, dir: dir, expect: map[string]string{},
		hc: &http.Client{Timeout: 30 * time.Second}}
}

func (b *opencodeBus) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(b.port) + path
}

func (b *opencodeBus) isConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// busEvent is what lux reads of a bus event.
type busEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			ParentID  string `json:"parentID"`
			SessionID string `json:"sessionID"`
		} `json:"info"`
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	} `json:"properties"`
}

// follow reads GET /event until ctx ends, reconnecting while the server is
// not up yet or the stream drops, and hands each event to on.
func (b *opencodeBus) follow(ctx context.Context, on func(busEvent)) {
	for ctx.Err() == nil {
		if err := b.followOnce(ctx, on); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}

func (b *opencodeBus) followOnce(ctx context.Context, on func(busEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url("/event"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-opencode-directory", b.dir)
	// No timeout: the stream is long-lived; ctx ends it.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /event: %s", resp.Status)
	}
	b.mu.Lock()
	b.connected = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.connected = false
		b.mu.Unlock()
	}()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		var ev busEvent
		if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil && ev.Type != "" {
			on(ev)
		}
	}
	return sc.Err()
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// messageID is a new ascending OpenCode message id: "msg_", 12 hex digits
// of (unix ms × 0x1000 + a per-ms counter), then 14 random base62
// characters. OpenCode orders messages by id, so it must sort after the
// messages already stored.
func (b *opencodeBus) messageID(now time.Time) string {
	b.mu.Lock()
	ms := now.UnixMilli()
	if ms > b.lastMs {
		b.lastMs, b.seq = ms, 0
	}
	b.seq++
	v := (b.lastMs*0x1000 + b.seq) & (1<<48 - 1)
	b.mu.Unlock()
	suffix := make([]byte, 14)
	for i := range suffix {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(base62))))
		suffix[i] = base62[n.Int64()]
	}
	return fmt.Sprintf("msg_%012x%s", v, suffix)
}

// errNotSent: the steer certainly did not reach OpenCode; another path
// may deliver it.
var errNotSent = errors.New("not sent")

// promptAsync stores text as the user message msgID of session and joins
// it to the running loop (starting one if none runs). An error wrapping
// errNotSent means OpenCode did not take it.
func (b *opencodeBus) promptAsync(session, msgID, text string) error {
	body, _ := json.Marshal(map[string]any{"messageID": msgID, "parts": textInput(text)})
	req, err := http.NewRequest(http.MethodPost, b.url("/session/"+session+"/prompt_async"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errNotSent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-directory", b.dir)
	resp, err := b.hc.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: %v", errNotSent, err)
		}
		return fmt.Errorf("prompt_async: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: prompt_async: %s", errNotSent, resp.Status)
	}
	return nil
}

func (b *opencodeBus) track(msgID, requestID string) {
	b.mu.Lock()
	b.expect[msgID] = requestID
	b.mu.Unlock()
}

func (b *opencodeBus) untrack(msgID string) {
	b.mu.Lock()
	delete(b.expect, msgID)
	b.mu.Unlock()
}

// answered returns the request id whose message an assistant step with
// this parentID answers, once.
func (b *opencodeBus) answered(parentID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.expect[parentID]
	delete(b.expect, parentID)
	return id, ok
}
