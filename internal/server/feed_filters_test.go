package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
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

// feedIDs is the ids of the lux events of an SSE body, in order.
func feedIDs(t *testing.T, body string) []int64 {
	t.Helper()
	ids := []int64{}
	for _, line := range strings.Split(body, "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var e FeedEvent
			if err := json.Unmarshal([]byte(d), &e); err != nil {
				t.Fatalf("%q: %v", d, err)
			}
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// feedQuery is the follow=false feed after start, with query q, as key.
func feedQuery(t *testing.T, s *Server, key string, start int64, q string) []int64 {
	t.Helper()
	return feedIDs(t, feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d&%s", start, q)))
}

func label(kv string) string { return "label=" + url.QueryEscape(kv) }

func wantIDs(t *testing.T, what string, got []int64, want ...int64) {
	t.Helper()
	if want == nil {
		want = []int64{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

// label= matches the event's Run's labels as GET /v1/runs?label= does:
// each label must match; an empty value is a value, even first.
func TestFeedLabelFilter(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	b := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(r1), nil, "state", `{}`, at)
	a2 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), a, a2)
	wantIDs(t, "team=y", feedQuery(t, s, key, start, label("team=y")), b)
	wantIDs(t, "env=prod", feedQuery(t, s, key, start, label("env=prod")), a, b, a2)
	wantIDs(t, "team=y env=prod", feedQuery(t, s, key, start, label("team=y")+"&"+label("env=prod")), b)
	wantIDs(t, "team=x env=dev", feedQuery(t, s, key, start, label("team=x")+"&"+label("env=dev")))
	wantIDs(t, "nope=x", feedQuery(t, s, key, start, label("nope=x")))
	// As on /v1/runs: blank= is the value "", and a first empty value
	// does not drop the others.
	runs := func(q string) []string {
		var out struct{ Runs []Run }
		w := apiCall(t, s, key, http.MethodGet, "/v1/runs?"+q, nil)
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("runs %s: %d %s", q, w.Code, w.Body)
		}
		ids := []string{}
		for _, r := range out.Runs {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := runs(label("blank=")); !slices.Equal(got, []string{feedRunA}) {
		t.Fatalf("/v1/runs blank=: %v", got)
	}
	wantIDs(t, "blank=", feedQuery(t, s, key, start, label("blank=")), a, a2)
	if got := runs(label("blank=") + "&" + label("team=y")); len(got) != 0 {
		t.Fatalf("/v1/runs blank= team=y: %v", got)
	}
	wantIDs(t, "blank= team=y", feedQuery(t, s, key, start, label("blank=")+"&"+label("team=y")))
	wantIDs(t, "team=", feedQuery(t, s, key, start, label("team=")))
	// An empty first label (key "", value "") matches no Run, and is not
	// dropped with those after it.
	if got := runs("label=&" + label("env=prod")); len(got) != 0 {
		t.Fatalf("/v1/runs label= env=prod: %v", got)
	}
	wantIDs(t, "label= env=prod", feedQuery(t, s, key, start, "label=&"+label("env=prod")))
}

// type= is a comma-separated list of exact types; with label=, both apply.
func TestFeedTypeFilter(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	st := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	ex := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "input", `{}`, at)
	ic := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "input.consumed", `{}`, at)
	bst := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	wantIDs(t, "state", feedQuery(t, s, key, start, "type=state"), st, bst)
	wantIDs(t, "state,exited,input.consumed", feedQuery(t, s, key, start, "type=state,exited,input.consumed"), st, ex, ic, bst)
	wantIDs(t, "input. is not a prefix", feedQuery(t, s, key, start, "type=input."))
	wantIDs(t, "team=x type=state,input.consumed", feedQuery(t, s, key, start, label("team=x")+"&type=state,input.consumed"), st)
	wantIDs(t, "team=y type=state,input.consumed", feedQuery(t, s, key, start, label("team=y")+"&type=state,input.consumed"), ic, bst)
}

// A server's event matches a label filter through its attached Run's
// labels, never while it has none; a type filter alone matches it.
func TestFeedLabelFilterServerEvents(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	loose := feedEvent(t, s, ctx, "t1", nil, ptr("srv_loose"), "server.state", `{"host": "web"}`, at)
	onA := feedEvent(t, s, ctx, "t1", ptr(feedRunA), ptr("srv_a"), "server.state", `{"host": "a"}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunB), ptr("srv_b"), "server.state", `{"host": "b"}`, at)
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), onA)
	wantIDs(t, "team=", feedQuery(t, s, key, start, label("team=")))
	wantIDs(t, "type=server.state team=x", feedQuery(t, s, key, start, "type=server.state&"+label("team=x")), onA)
	wantIDs(t, "last=5 team=x", feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&last=5&"+label("team=x"))), onA)
	body := feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d&type=server.state", start))
	if got := feedIDs(t, body); len(got) != 3 || got[0] != loose {
		t.Fatalf("type=server.state: %v", got)
	}
	// A sent server event has its detail, as unfiltered.
	if !strings.Contains(body, `"url":"https://web.lux.example.com"`) {
		t.Fatalf("detail: %s", body)
	}
}

// More than a window of settled events that do not match, then one that
// does: the feed passes them all, one window after the other, and sends
// it.
func TestFeedWatermarkPassesUnmatchedEvents(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data, created_at)
		SELECT 't1', $1, 1, 'state', '{}', now() - interval '1 hour' FROM generate_series(1, $2::int)`, feedRunB, feedPage+100)
	var lastSkipped int64
	systemScan(t, s, `SELECT max(id) FROM run_events`, nil, &lastSkipped)
	match := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, "2026-01-02T03:04:05Z")
	type read struct {
		after, last int64
		n           int
	}
	var reads []read
	feedRead = func(s *Server, ctx context.Context, p Principal, after int64, f feedFilter) ([]feedRow, error) {
		rows, err := s.feedWindow(ctx, p, after, f)
		r := read{after: after, n: len(rows)}
		if len(rows) > 0 {
			r.last = rows[len(rows)-1].ID
		}
		reads = append(reads, r)
		return rows, err
	}
	t.Cleanup(func() { feedRead = (*Server).feedWindow })
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), match)
	// Window one: feedPage unmatched; window two from its last, through
	// the rest and the match.
	if len(reads) != 2 || reads[0].after != start || reads[0].n != feedPage ||
		reads[1].after != reads[0].last || reads[1].n != 101 || reads[1].last != match || match != lastSkipped+1 {
		t.Fatalf("reads: %+v (start %d, last skipped %d, match %d)", reads, start, lastSkipped, match)
	}
}

// last=N with a filter starts at the N newest matching events; fewer
// match: all of them; none: from now.
func TestFeedLastWithFilter(t *testing.T) {
	s, ctx, key, _, _ := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	var a, bx []int64
	for range 3 {
		a = append(a, feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at))
		feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
		bx = append(bx, feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "exited", `{}`, at))
	}
	ax := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	last := func(q string) []int64 {
		return feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&"+q))
	}
	wantIDs(t, "last=2 team=x", last("last=2&"+label("team=x")), a[2], ax)
	wantIDs(t, "last=1 team=x", last("last=1&"+label("team=x")), ax)
	wantIDs(t, "last=9 team=x", last("last=9&"+label("team=x")), a[0], a[1], a[2], ax)
	wantIDs(t, "last=2 type=state team=x", last("last=2&type=state&"+label("team=x")), a[1], a[2])
	wantIDs(t, "last=2 type=exited", last("last=2&type=exited"), bx[2], ax)
	wantIDs(t, "last=3 type=exited team=y", last("last=3&type=exited&"+label("team=y")), bx...)
	wantIDs(t, "last=5 team=nope", last("last=5&"+label("team=nope")))
	wantIDs(t, "last=0 team=x", last("last=0&"+label("team=x")))
}

// A filter narrows what the caller sees, never widens it: t2's feed, its
// labels matching t1's Runs, has none of t1's events, nor t1's of t2's.
func TestFeedFiltersKeepTenants(t *testing.T) {
	s, ctx, key, key2, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	o := feedEvent(t, s, ctx, "t2", ptr(feedRunO), nil, "state", `{}`, at)
	for _, q := range []string{label("team=x"), "type=state", label("team=x") + "&type=state"} {
		wantIDs(t, "t1 "+q, feedQuery(t, s, key, start, q), a)
		wantIDs(t, "t2 "+q, feedQuery(t, s, key2, start, q), o)
	}
	q := "/v1/events?follow=false&last=5&" + label("team=x")
	wantIDs(t, "t1 last", feedIDs(t, feedBody(t, s, key, q)), a)
	wantIDs(t, "t2 last", feedIDs(t, feedBody(t, s, key2, q)), o)
}
