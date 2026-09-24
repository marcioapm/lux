package server

import (
	"context"
	"testing"
	"time"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A follower takes its channel before it reads: an event during the read
// still wakes it. Run followers wake only for their Run (or for "every").
func TestWakeups(t *testing.T) {
	w := newWakeups()
	all, a, b := w.next(""), w.next("a"), w.next("b")
	w.notify("a")
	if !closed(all) || !closed(a) || closed(b) {
		t.Fatalf("event of a: all %v a %v b %v", closed(all), closed(a), closed(b))
	}
	if closed(w.next("a")) || closed(w.next("")) {
		t.Fatal("the next channels are already closed")
	}
	w.notify("")
	if !closed(b) {
		t.Fatal("a reconnect wakes every Run follower")
	}
	start := time.Now()
	wait(context.Background(), w.next(""), 20*time.Millisecond)
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("wait returned before the fallback with no event")
	}
}
