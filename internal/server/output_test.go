package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// fakeOutputHost stands in for a host's runner connection, answering output
// subscriptions as a real runner does: for a placement it does not hold, an
// output.end at once with no error; for one it holds, its records after
// since, then an end if it has exited, else the subscription stays open.
type fakeOutputHost struct {
	mu   sync.Mutex
	held map[int]fakePlacement
	subs map[int]int // subscriptions received, by epoch
}

type fakePlacement struct {
	data   []string // record i has seq i+1
	exited bool
	// ready, if set, is whether the runner holds it yet, asked at each
	// subscription.
	ready func() bool
}

func newFakeOutputHost(t *testing.T, s *Server, hostID string) *fakeOutputHost {
	t.Helper()
	h := &fakeOutputHost{held: map[int]fakePlacement{}, subs: map[int]int{}}
	c := &runnerConn{hostID: hostID, send: make(chan proto.Frame, 256), notify: make(chan struct{}, 1), done: make(chan struct{})}
	s.hub.mu.Lock()
	s.hub.conns[hostID] = c
	s.hub.mu.Unlock()
	t.Cleanup(func() { close(c.done) })
	go func() {
		for {
			select {
			case <-c.done:
				return
			case f := <-c.send:
				if f.Type == proto.MsgOutputSubscribe {
					h.answer(s, f)
				}
			}
		}
	}()
	return h
}

func (h *fakeOutputHost) hold(epoch int, exited bool, data ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[epoch] = fakePlacement{data: data, exited: exited}
}

func (h *fakeOutputHost) holdWhen(epoch int, ready func() bool, data ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[epoch] = fakePlacement{data: data, ready: ready}
}

func (h *fakeOutputHost) subscribed(epoch int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subs[epoch]
}

func (h *fakeOutputHost) answer(s *Server, f proto.Frame) {
	var sub proto.OutputSubscribe
	_ = json.Unmarshal(f.Data, &sub)
	h.mu.Lock()
	h.subs[f.Epoch]++
	p, ok := h.held[f.Epoch]
	h.mu.Unlock()
	if ok && p.ready != nil && !p.ready() {
		p, ok = fakePlacement{}, false
	}
	var recs []proto.Record
	for i, d := range p.data {
		if seq := int64(i + 1); seq > sub.Since {
			recs = append(recs, proto.Record{Seq: seq, Ch: "stdout", Data: d})
		}
	}
	if len(recs) > 0 {
		s.hub.route(sub.SubID, proto.Frame{Type: proto.MsgOutputRecords, RunID: f.RunID, Epoch: f.Epoch,
			Data: proto.Marshal(proto.OutputRecords{SubID: sub.SubID, Records: recs})})
	}
	if !ok || p.exited {
		s.hub.route(sub.SubID, proto.Frame{Type: proto.MsgOutputEnd, RunID: f.RunID, Epoch: f.Epoch,
			Data: proto.Marshal(proto.OutputEnd{SubID: sub.SubID})})
	}
}

type outputSSE struct {
	name, data string
}

// followOutput GETs r1's output with follow and returns its SSE events,
// closed at EOF. The request starts in the background: with nothing to
// send yet, its response headers are not flushed.
func followOutput(t *testing.T, s *Server, key string) <-chan outputSSE {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(func() { cancel(); srv.Close() })
	events := make(chan outputSSE, 64)
	go func() {
		defer close(events)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/r1/output?follow=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			events <- outputSSE{"transport", err.Error()}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			events <- outputSSE{"status", resp.Status}
			return
		}
		var ev outputSSE
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				events <- ev
				ev = outputSSE{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if err := sc.Err(); err != nil {
			events <- outputSSE{"transport", err.Error()}
		}
	}()
	return events
}

// nextEvent returns the stream's next event; ok is false at EOF or after
// within.
func nextEvent(events <-chan outputSSE, within time.Duration) (ev outputSSE, ok bool) {
	select {
	case ev, ok = <-events:
		return ev, ok
	case <-time.After(within):
		return outputSSE{}, false
	}
}

func expectRecord(t *testing.T, events <-chan outputSSE, cursor, data, what string) {
	t.Helper()
	ev, ok := nextEvent(events, 5*time.Second)
	var r OutputRecord
	if ok && ev.name == "record" {
		_ = json.Unmarshal([]byte(ev.data), &r)
	}
	if !ok || ev.name != "record" || r.Cursor != cursor || r.Data != data {
		t.Fatalf("%s: got %+v (ok %v), want record %s %q", what, ev, ok, cursor, data)
	}
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// resumedFixture: r1 resumed on h1, its exited placement 1 having written
// "one\n" and its placement 2 assigned.
func resumedFixture(t *testing.T) (*Server, string, *fakeOutputHost) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled', current_epoch = 2 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'assigned')`)
	h1 := newFakeOutputHost(t, s, "h1")
	h1.hold(1, true, "one\n")
	return s, keys["t1"], h1
}

// A follower that reaches a resumed Run's new placement before its runner
// has taken it up waits for it, and gets its records once it runs.
func TestFollowWaitsForAnAssignedPlacement(t *testing.T) {
	s, key, h1 := resumedFixture(t)
	ctx := context.Background()
	events := followOutput(t, s, key)
	expectRecord(t, events, "1.1", "one\n", "first record")
	// Asked, and answered "nothing", more than once while assigned; but
	// about every 500ms, not in a tight loop.
	waitUntil(t, func() bool { return h1.subscribed(2) >= 2 }, "the follower to ask for placement 2 twice")
	before := h1.subscribed(2)
	time.Sleep(time.Second)
	if n := h1.subscribed(2) - before; n > 4 {
		t.Fatalf("asked for the assigned placement %d times in a second", n)
	}
	h1.hold(2, false, "two\n")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	expectRecord(t, events, "2.1", "two\n", "the new placement's record")
}

// Run events arriving while placement 2 is assigned and its runner does not
// hold it wake the follower, but do not make it ask again more than once
// per 500ms.
func TestFollowAsksForAnAssignedPlacementAtMostTwiceASecondUnderEvents(t *testing.T) {
	s, key, h1 := resumedFixture(t)
	events := followOutput(t, s, key)
	expectRecord(t, events, "1.1", "one\n", "first record")
	waitUntil(t, func() bool { return h1.subscribed(2) >= 1 }, "the follower to ask for placement 2")
	before, start := h1.subscribed(2), time.Now()
	for time.Since(start) < 1500*time.Millisecond {
		s.wakeups.notify("r1")
		time.Sleep(2 * time.Millisecond)
	}
	n, elapsed := h1.subscribed(2)-before, time.Since(start)
	t.Logf("%d subscriptions in %s", n, elapsed)
	// One per started 500ms, plus one in flight at either end.
	if allowed := int(elapsed/(500*time.Millisecond)) + 2; n > allowed {
		t.Fatalf("asked for the assigned placement %d times in %s under events (at most %d)", n, elapsed, allowed)
	}
	h1.hold(2, false, "two\n")
	expectRecord(t, events, "2.1", "two\n", "the placement's record once its runner holds it")
}

// The runner holds placement 2 and writes its output, but luxd still has it
// assigned (its starting report was refused, say): the output flows.
func TestFollowReadsAnAssignedPlacementItsRunnerHolds(t *testing.T) {
	s, key, h1 := resumedFixture(t)
	h1.hold(2, false, "two\n")
	events := followOutput(t, s, key)
	expectRecord(t, events, "1.1", "one\n", "first record")
	expectRecord(t, events, "2.1", "two\n", "the held placement's record, its row still assigned")
}

// Output a starting placement has comes before it is running. The fake
// runner holds placement 2 from when its row leaves assigned, so the record
// cannot have come while it was assigned.
func TestFollowReadsAStartingPlacement(t *testing.T) {
	s, key, h1 := resumedFixture(t)
	ctx := context.Background()
	h1.holdWhen(2, func() bool { st, err := placementState(s, "p2"); return err == nil && st != "assigned" }, "two\n")
	events := followOutput(t, s, key)
	expectRecord(t, events, "1.1", "one\n", "first record")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'starting' WHERE id = 'p2'`)
	expectRecord(t, events, "2.1", "two\n", "the starting placement's record")
	if st, err := placementState(s, "p2"); err != nil || st != "starting" {
		t.Fatalf("p2 is %s (%v)", st, err)
	}
}

// placementState is safe to call off the test's goroutine.
func placementState(s *Server, id string) (st string, err error) {
	ctx := context.Background()
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM placements WHERE id = $1`, id).Scan(&st)
	})
	return st, err
}

// An assigned placement that never starts loses its lease: the follower
// waiting on it is told and its stream ends.
func TestFollowEndsWhenAnAssignedPlacementIsLost(t *testing.T) {
	s, key, h1 := resumedFixture(t)
	ctx := context.Background()
	events := followOutput(t, s, key)
	expectRecord(t, events, "1.1", "one\n", "first record")
	waitUntil(t, func() bool { return h1.subscribed(2) >= 1 }, "the follower to ask for placement 2")
	execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 minute' WHERE id = 'p2'`)
	if err := s.reapLeases(ctx); err != nil {
		t.Fatal(err)
	}

	ev, ok := nextEvent(events, 5*time.Second)
	var gap outputGap
	if ok && ev.name == "gap" {
		_ = json.Unmarshal([]byte(ev.data), &gap)
	}
	if !ok || ev.name != "gap" || gap.Epoch != 2 {
		t.Fatalf("after the lease expired: %+v (ok %v), want a gap for epoch 2", ev, ok)
	}
	ev, ok = nextEvent(events, 5*time.Second)
	var end outputEnd
	if ok && ev.name == "end" {
		_ = json.Unmarshal([]byte(ev.data), &end)
	}
	if !ok || ev.name != "end" || end.State != StateLost || end.Cursor != "3.0" {
		t.Fatalf("after the gap: %+v (ok %v), want end, lost at 3.0", ev, ok)
	}
	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("after the end: %+v, want EOF", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not close after its end")
	}
}

// Followed from submit: the first placement is assigned and its runner
// does not hold it yet; its records still reach the follower.
func TestFollowAFirstPlacementFromSubmit(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled', current_epoch = 1 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'assigned')`)
	h1 := newFakeOutputHost(t, s, "h1")
	events := followOutput(t, s, keys["t1"])
	waitUntil(t, func() bool { return h1.subscribed(1) >= 1 }, "the follower to ask for placement 1")
	h1.hold(1, false, "one\n")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	expectRecord(t, events, "1.1", "one\n", "the first placement's record")
}

// Resumed on another host: h1 has the exited placement 1, h2 is assigned
// placement 2 and takes it up later.
func TestFollowAcrossAMove(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'scheduled', current_epoch = 2 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready'), ('h2', 'h2', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h2', 2, 'assigned')`)
	h1 := newFakeOutputHost(t, s, "h1")
	h1.hold(1, true, "one\n", "uno\n")
	h2 := newFakeOutputHost(t, s, "h2")
	events := followOutput(t, s, keys["t1"])
	expectRecord(t, events, "1.1", "one\n", "placement 1, record 1")
	expectRecord(t, events, "1.2", "uno\n", "placement 1, record 2")
	waitUntil(t, func() bool { return h2.subscribed(2) >= 1 }, "the follower to ask h2 for placement 2")
	h2.hold(2, false, "two\n", "dos\n")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	expectRecord(t, events, "2.1", "two\n", "placement 2, record 1")
	expectRecord(t, events, "2.2", "dos\n", "placement 2, record 2")
	if n := h1.subscribed(2); n != 0 {
		t.Fatalf("h1 was asked for placement 2 %d times", n)
	}
}
