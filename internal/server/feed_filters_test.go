package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

const (
	feedRunA = "run_feedaaaaaaaaaaaa"
	feedRunB = "run_feedbbbbbbbbbbbb"
	feedRunO = "run_feedoooooooooooo" // tenant t2's
)

// feedFixture: wakeFixture plus Runs of t1 with labels (A: team=x env=prod,
// B: team=y, r1: none) and one of t2 (team=x), and the highest event id
// before the test's own.
func feedFixture(t *testing.T) (s *Server, ctx context.Context, key, key2 string, start int64) {
	t.Helper()
	s, ctx, key, key2 = wakeFixture(t)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, labels) VALUES
		($1, 't1', '{}', 'running', 1, '{"team": "x", "env": "prod", "blank": ""}'),
		($2, 't1', '{}', 'running', 1, '{"team": "y", "env": "prod"}'),
		($3, 't2', '{}', 'running', 1, '{"team": "x", "env": "prod"}')`, feedRunA, feedRunB, feedRunO)
	systemScan(t, s, `SELECT coalesce(max(id), 0) FROM run_events`, nil, &start)
	return s, ctx, key, key2, start
}

// feedEvent inserts an event of a Run (runID) or a server (serverID), at a
// fixed time, and returns its id.
func feedEvent(t *testing.T, s *Server, ctx context.Context, tenant string, runID, serverID *string, typ, data, at string) int64 {
	t.Helper()
	var id int64
	systemScan(t, s, `INSERT INTO run_events (tenant_id, run_id, server_id, epoch, type, data, created_at)
		VALUES ($1, $2, $3, CASE WHEN $3::text IS NULL THEN 1 END, $4, $5::jsonb, $6::timestamptz) RETURNING id`,
		[]any{tenant, runID, serverID, typ, data, at}, &id)
	return id
}

func ptr[T any](v T) *T { return &v }

// feedBody is GET target through the route, follow=false, as key.
func feedBody(t *testing.T, s *Server, key, target string) string {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, target, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, w.Code, w.Body)
	}
	return w.Body.String()
}

// Without label or type the feed's SSE body is exactly what it was: every
// event the tenant sees after the watermark, server events with their
// hostname and url, nothing of another tenant's; last=N the N newest.
func TestFeedUnfilteredBodyIsPinned(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a1 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{"to": "running"}`, at)
	feedEvent(t, s, ctx, "t2", ptr(feedRunO), nil, "state", `{"to": "running"}`, at)
	b1 := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "exited", `{"code": 0}`, at)
	sv := feedEvent(t, s, ctx, "t1", nil, ptr("srv_pinned"), "server.state", `{"host": "web", "name": "web", "runId": null}`, at)
	want := fmt.Sprintf(`id: %d
event: lux
data: {"id":%d,"epoch":1,"type":"state","data":{"to":"running"},"time":"2026-01-02T03:04:05Z","runId":"%s","tenant":"t1"}

id: %d
event: lux
data: {"id":%d,"epoch":1,"type":"exited","data":{"code":0},"time":"2026-01-02T03:04:05Z","runId":"%s","tenant":"t1"}

id: %d
event: lux
data: {"id":%d,"serverId":"srv_pinned","type":"server.state","data":{"host":"web","hostname":"web.lux.example.com","name":"web","runId":null,"url":"https://web.lux.example.com"},"time":"2026-01-02T03:04:05Z","runId":null,"tenant":"t1"}

`, a1, a1, feedRunA, b1, b1, feedRunB, sv, sv)
	if got := feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d", start)); got != want {
		t.Fatalf("after:\n%s\nwant:\n%s", got, want)
	}
	wantLast := want[len(fmt.Sprintf("id: %d\nevent: lux\ndata: {\"id\":%d,\"epoch\":1,\"type\":\"state\",\"data\":{\"to\":\"running\"},\"time\":\"2026-01-02T03:04:05Z\",\"runId\":\"%s\",\"tenant\":\"t1\"}\n\n", a1, a1, feedRunA)):]
	if got := feedBody(t, s, key, "/v1/events?follow=false&last=2"); got != wantLast {
		t.Fatalf("last=2:\n%s\nwant:\n%s", got, wantLast)
	}
}
