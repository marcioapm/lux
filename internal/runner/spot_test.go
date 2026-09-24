package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInstanceAction(t *testing.T) {
	notice := ""
	tokens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			tokens++
			w.Write([]byte("tok"))
		case r.Header.Get("X-aws-ec2-metadata-token") != "tok":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/latest/meta-data/spot/instance-action" && notice != "":
			w.Write([]byte(notice))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m := &imds{base: srv.URL, http: srv.Client()}
	ctx := context.Background()

	if action, _, err := m.instanceAction(ctx); err != nil || action != "" {
		t.Fatalf("no notice: got %q, %v", action, err)
	}
	notice = `{"action": "terminate", "time": "2026-09-24T08:22:00Z"}`
	action, at, err := m.instanceAction(ctx)
	if err != nil || action != "terminate" || !at.Equal(time.Date(2026, 9, 24, 8, 22, 0, 0, time.UTC)) {
		t.Fatalf("notice: got %q %v, %v", action, at, err)
	}
	if tokens != 1 {
		t.Errorf("the session token is reused: %d tokens", tokens)
	}
	notice = `{}`
	if _, _, err := m.instanceAction(ctx); err == nil {
		t.Error("a notice without an action is an error")
	}
}

func TestEvictionGrace(t *testing.T) {
	r := &Runner{}
	if g := r.evictionGrace(30 * time.Second); g != 30*time.Second {
		t.Errorf("not evicting: %v", g)
	}
	at := time.Now().Add(20 * time.Second)
	r.evictBy.Store(&at)
	if g := r.evictionGrace(30 * time.Second); g > 10*time.Second || g < 9*time.Second {
		t.Errorf("evicting in 20s: grace %v, want about half", g)
	}
	past := time.Now().Add(-time.Second)
	r.evictBy.Store(&past)
	if g := r.evictionGrace(30 * time.Second); g != time.Second {
		t.Errorf("past the deadline: %v", g)
	}
}
