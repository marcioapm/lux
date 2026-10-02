package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// fakeOutputRunner stands in for h1's runner connection, answering output
// subscriptions as a real runner does: epoch 1 has exited with one record;
// epoch 2, until held is set, is unknown to it (it ends the subscription
// at once, with no error), and after that has one record and stays open.
func fakeOutputRunner(t *testing.T, s *Server, held *atomic.Bool) {
	t.Helper()
	c := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 16), notify: make(chan struct{}, 1), done: make(chan struct{})}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = c
	s.hub.mu.Unlock()
	t.Cleanup(func() { close(c.done) })
	records := func(sub string, epoch int, data string) {
		s.hub.route(sub, proto.Frame{Type: proto.MsgOutputRecords, RunID: "r1", Epoch: epoch,
			Data: proto.Marshal(proto.OutputRecords{SubID: sub, Records: []proto.Record{{Seq: 1, Ch: "stdout", Data: data}}})})
	}
	end := func(sub string, epoch int) {
		s.hub.route(sub, proto.Frame{Type: proto.MsgOutputEnd, RunID: "r1", Epoch: epoch, Data: proto.Marshal(proto.OutputEnd{SubID: sub})})
	}
	go func() {
		for {
			select {
			case <-c.done:
				return
			case f := <-c.send:
				if f.Type != proto.MsgOutputSubscribe {
					continue
				}
				var sub proto.OutputSubscribe
				_ = json.Unmarshal(f.Data, &sub)
				switch {
				case f.Epoch == 1:
					if sub.Since < 1 {
						records(sub.SubID, 1, "one\n")
					}
					end(sub.SubID, 1)
				case f.Epoch == 2 && held.Load():
					if sub.Since < 1 {
						records(sub.SubID, 2, "two\n")
					}
				default:
					end(sub.SubID, f.Epoch)
				}
			}
		}
	}()
}

// A follower that reaches a resumed Run's new placement before its runner
// has taken it up waits for it, and gets its records once it runs.
func TestFollowWaitsForAnAssignedPlacement(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled', current_epoch = 2 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'assigned')`)
	var held atomic.Bool
	fakeOutputRunner(t, s, &held)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, srv.URL+"/v1/runs/r1/output?follow=true", nil)
	req.Header.Set("Authorization", "Bearer "+keys["t1"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	recs := make(chan OutputRecord, 16)
	go func() {
		defer close(recs)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var r OutputRecord
				if json.Unmarshal([]byte(data), &r) == nil && r.Cursor != "" {
					recs <- r
				}
			}
		}
	}()
	next := func(within time.Duration) (OutputRecord, bool) {
		select {
		case r, ok := <-recs:
			return r, ok
		case <-time.After(within):
			return OutputRecord{}, false
		}
	}
	if r, ok := next(5 * time.Second); !ok || r.Cursor != "1.1" || r.Data != "one\n" {
		t.Fatalf("first record: %+v %v", r, ok)
	}
	// The follower polls the assigned placement a few times (every 500ms)
	// before its runner takes it up.
	time.Sleep(1500 * time.Millisecond)
	held.Store(true)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	if r, ok := next(5 * time.Second); !ok || r.Cursor != "2.1" || r.Data != "two\n" {
		t.Fatalf("the new placement's record never came: %+v %v", r, ok)
	}
}
