package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Interactive access: GET /v1/runs/{id}/exec, /attach and /ports/{name}
// upgrade to a WebSocket, relayed to the Run's host.
//
// Client protocol (JSON text messages):
//   - exec: the client sends one proto.StreamOpen first ({"command":[...],
//     "tty":true,"rows":..,"cols":..}); attach and ports need none.
//   - then proto.StreamData both ways: {"data":<base64>} for bytes,
//     {"eof":true} to close input, {"rows","cols"} to resize;
//   - luxd ends with one {"exitCode":N} or {"error":"..."} and closes.
//
// A stream ends with its WebSocket; nothing is replayed.

func (s *Server) streamHandler(kind string) handler {
	return func(w http.ResponseWriter, r *http.Request) error { return s.streamRun(w, r, kind) }
}

func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, kind string) error {
	p := principal(r)
	runID := r.PathValue("id")
	var hostID string
	var epoch, port int
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		run, err := s.loadRun(r.Context(), p.TenantID, runID, false)
		if err != nil {
			return err
		}
		if run.State != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: interactive access needs it running", run.State)
		}
		switch kind {
		case "attach":
			if !run.Spec.Workload.TTY {
				return errf(http.StatusConflict, "no_terminal", "attach needs a generic workload with workload.tty")
			}
		case "tunnel":
			name := r.PathValue("name")
			for _, dp := range run.Spec.Network.Ports {
				if dp.Name == name {
					port = dp.Port
				}
			}
			if port == 0 {
				return errf(http.StatusNotFound, "not_found", "the Run declares no port named %q", name)
			}
		}
		epoch = run.Epoch
		return tx.QueryRow(r.Context(), `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&hostID)
	})
	if err != nil {
		return err
	}
	if !s.hub.Streaming(hostID) {
		return errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil
	}
	ws.SetReadLimit(4 << 20)
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	open := proto.StreamOpen{Kind: kind, Port: port}
	if kind == "exec" {
		if err := wsRead(ctx, ws, &open); err != nil || len(open.Command) == 0 {
			closeWith(ctx, ws, proto.StreamData{Error: "exec needs a command"})
			return nil
		}
		open.Kind, open.Port = kind, 0
	}
	open.StreamID = ids.New("st")
	ch, unsub := s.hub.Subscribe(open.StreamID)
	defer unsub()
	send := func(typ string, d proto.StreamData) error {
		d.StreamID = open.StreamID
		return s.hub.SendLive(hostID, proto.Frame{Type: typ, RunID: runID, Epoch: epoch, Data: proto.Marshal(d)})
	}
	if err := s.hub.SendLive(hostID, proto.Frame{Type: proto.MsgStreamOpen, RunID: runID, Epoch: epoch, Data: proto.Marshal(open)}); err != nil {
		closeWith(ctx, ws, proto.StreamData{Error: err.Error()})
		return nil
	}
	defer send(proto.MsgStreamClose, proto.StreamData{})

	// Client → host.
	go func() {
		defer cancel()
		for {
			var d proto.StreamData
			if err := wsRead(ctx, ws, &d); err != nil {
				return
			}
			d.ExitCode, d.Error = nil, ""
			if err := send(proto.MsgStreamData, d); err != nil {
				return
			}
		}
	}()
	// Host → client.
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-ch:
			if !ok {
				closeWith(ctx, ws, proto.StreamData{Error: "the stream fell behind and was dropped"})
				return nil
			}
			var d proto.StreamData
			_ = json.Unmarshal(f.Data, &d)
			d.StreamID = ""
			if f.Type == proto.MsgStreamClose {
				closeWith(ctx, ws, d)
				return nil
			}
			if err := wsWrite(ctx, ws, d); err != nil {
				return nil
			}
		}
	}
}

func wsRead(ctx context.Context, ws *websocket.Conn, v any) error {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func wsWrite(ctx context.Context, ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

// closeWith sends the stream's last message and closes the socket.
func closeWith(ctx context.Context, ws *websocket.Conn, d proto.StreamData) {
	if d.ExitCode == nil && d.Error == "" {
		code := 0
		d.ExitCode = &code
	}
	_ = wsWrite(ctx, ws, d)
	_ = ws.Close(websocket.StatusNormalClosure, "")
}
