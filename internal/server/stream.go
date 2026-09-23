package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Interactive access: GET /v1/runs/{id}/exec, /attach and /ports/{name}
// upgrade to a WebSocket, relayed to the Run's host.
//
// Client protocol (JSON text messages, proto.StreamData):
//   - exec: the client sends one proto.StreamOpen first ({"command":[...],
//     "tty":true,"rows":..,"cols":..}); attach and ports need none.
//   - then StreamData both ways: {"data":<base64>} for bytes, {"eof":true}
//     to close input, {"rows","cols"} to resize;
//   - the stream ends with one {"exitCode":N} (exec) or {"error":"..."}, or
//     just the socket closing (a tunnel whose connection ended), and luxd
//     closes.
//
// Without the WebSocket upgrade, the same URL answers 200 if the stream
// could be opened and the error it would get otherwise, so a client can
// check first (lux port-forward does, before listening).

type streamTarget struct {
	hostID string
	epoch  int
	port   int
}

// resolveStream checks a stream can be opened and finds where it goes.
func (s *Server) resolveStream(r *http.Request, kind string) (streamTarget, error) {
	p := principal(r)
	runID := r.PathValue("id")
	var t streamTarget
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var sp spec.RunSpec
		err := tx.QueryRow(r.Context(), `SELECT r.state, r.spec, r.current_epoch, coalesce(p.host_id, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, runID).Scan(&state, &sp, &t.epoch, &t.hostID)
		if err != nil {
			return err
		}
		if state != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: interactive access needs it running", state)
		}
		switch kind {
		case "attach":
			if !sp.Workload.TTY {
				return errf(http.StatusConflict, "no_terminal", "attach needs a generic workload with workload.tty")
			}
		case "tunnel":
			name := r.PathValue("name")
			for _, dp := range sp.Network.Ports {
				if dp.Name == name {
					t.port = dp.Port
				}
			}
			if t.port == 0 {
				return errf(http.StatusNotFound, "not_found", "the Run declares no port named %q", name)
			}
		}
		return nil
	})
	if err != nil {
		return t, err
	}
	if !s.hub.Streaming(t.hostID) {
		return t, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
	}
	return t, nil
}

func (s *Server) streamHandler(kind string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		t, err := s.resolveStream(r, kind)
		if err != nil {
			return err
		}
		if r.Header.Get("Upgrade") == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return nil
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return nil
		}
		ws.SetReadLimit(4 << 20)
		defer ws.CloseNow()
		s.relayStream(r.Context(), ws, r.PathValue("id"), kind, t)
		return nil
	}
}

func (s *Server) relayStream(ctx context.Context, ws *websocket.Conn, runID, kind string, t streamTarget) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	open := proto.StreamOpen{Kind: kind, Port: t.port}
	if kind == "exec" {
		if err := wsjson.Read(ctx, ws, &open); err != nil || len(open.Command) == 0 {
			closeWith(ctx, ws, []byte(`{"error":"exec needs a command"}`))
			return
		}
		open.Kind, open.Port = kind, 0
	}
	id := ids.New("st")
	ch, unsub := s.hub.Subscribe(id)
	defer unsub()
	send := func(typ string, data []byte) error {
		return s.hub.SendLive(t.hostID, proto.Frame{Type: typ, RunID: runID, Epoch: t.epoch, Stream: id, Data: data})
	}
	if err := send(proto.MsgStreamOpen, proto.Marshal(open)); err != nil {
		closeWith(ctx, ws, proto.Marshal(proto.StreamData{Error: err.Error()}))
		return
	}
	defer send(proto.MsgStreamClose, nil)

	// Client → host: checked to be StreamData, forwarded as sent.
	go func() {
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var d proto.StreamData
			if json.Unmarshal(b, &d) != nil || d.ExitCode != nil || d.Error != "" {
				return
			}
			if err := send(proto.MsgStreamData, b); err != nil {
				return
			}
		}
	}()
	// Host → client, forwarded as they come.
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-ch:
			if !ok {
				closeWith(ctx, ws, []byte(`{"error":"the stream fell behind and was dropped"}`))
				return
			}
			if f.Type == proto.MsgStreamClose {
				closeWith(ctx, ws, f.Data)
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
			err := ws.Write(wctx, websocket.MessageText, f.Data)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// closeWith sends the stream's last message, if it has one, and closes.
func closeWith(ctx context.Context, ws *websocket.Conn, last []byte) {
	var d proto.StreamData
	if json.Unmarshal(last, &d) == nil && (d.ExitCode != nil || d.Error != "") {
		_ = ws.Write(ctx, websocket.MessageText, last)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
}
