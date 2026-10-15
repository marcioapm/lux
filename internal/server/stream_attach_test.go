package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marcioapm/lux/internal/proto"
)

// Attach's restore, rows and cols reach the host as the StreamOpen's;
// without them the StreamOpen asks for no restore, as before.
func TestAttachStreamOpenRestore(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload": {"workdir": "/work", "tty": true}}' WHERE id = $1`, r1)

	opens := make(chan proto.StreamOpen, 4)
	c := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 256), notify: make(chan struct{}, 1), done: make(chan struct{})}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = c
	s.hub.mu.Unlock()
	t.Cleanup(func() {
		s.hub.mu.Lock()
		delete(s.hub.conns, "h1")
		s.hub.mu.Unlock()
		close(c.done)
	})
	go func() {
		for {
			select {
			case <-c.done:
				return
			case f := <-c.send:
				if f.Type != proto.MsgStreamOpen {
					continue
				}
				var o proto.StreamOpen
				_ = json.Unmarshal(f.Data, &o)
				opens <- o
				s.hub.route(f.Stream, proto.Frame{Type: proto.MsgStreamData, Stream: f.Stream, Data: proto.Marshal(proto.StreamData{Channel: "stdout", Data: []byte("snapshot")})})
				s.hub.route(f.Stream, proto.Frame{Type: proto.MsgStreamClose, Stream: f.Stream, Data: proto.Marshal(proto.StreamData{Error: "the workload exited"})})
			}
		}
	}()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, tc := range []struct {
		query string
		want  proto.StreamOpen
	}{
		{"", proto.StreamOpen{Kind: "attach"}},
		{"?restore=true", proto.StreamOpen{Kind: "attach", Restore: true}},
		{"?restore=true&rows=30&cols=100", proto.StreamOpen{Kind: "attach", Restore: true, Rows: 30, Cols: 100}},
		{"?rows=30&cols=100", proto.StreamOpen{Kind: "attach"}}, // a size alone changes nothing
	} {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ws, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/runs/"+r1+"/attach"+tc.query,
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
		if err != nil {
			cancel()
			t.Fatalf("%q: %v", tc.query, err)
		}
		var got proto.StreamOpen
		select {
		case got = <-opens:
		case <-wctx.Done():
			t.Fatalf("%q: no StreamOpen", tc.query)
		}
		if got.Restore != tc.want.Restore || got.Rows != tc.want.Rows || got.Cols != tc.want.Cols || got.Kind != "attach" {
			t.Fatalf("%q: StreamOpen %+v, want %+v", tc.query, got, tc.want)
		}
		var d proto.StreamData
		if err := wsjson.Read(wctx, ws, &d); err != nil || string(d.Data) != "snapshot" {
			t.Fatalf("%q: first message %+v %v", tc.query, d, err)
		}
		ws.CloseNow()
		cancel()
	}
	if w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1+"/attach?rows=-1", nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative rows: %d %s", w.Code, w.Body)
	}
}
