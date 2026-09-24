package server

import (
	"context"
	"sync"
	"time"
)

// wakeups wake followers of Run events (the feed, output streams) when any
// is written, by any luxd: Postgres notifies lux_events after each insert
// into run_events (migration 012), and one connection per luxd listens.
// Followers re-read under their own scope; a wake-up says only "look".
type wakeups struct {
	mu   sync.Mutex
	wake chan struct{} // closed and replaced on each notification
}

func newWakeups() *wakeups { return &wakeups{wake: make(chan struct{})} }

// changed is closed at the next event.
func (e *wakeups) changed() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wake
}

func (e *wakeups) notify() {
	e.mu.Lock()
	close(e.wake)
	e.wake = make(chan struct{})
	e.mu.Unlock()
}

// wait returns at the next event, after fallback (a listener that lost
// its connection misses notifications until it is back), or when ctx ends.
func (e *wakeups) wait(ctx context.Context, fallback time.Duration) {
	select {
	case <-ctx.Done():
	case <-e.changed():
	case <-time.After(fallback):
	}
}

// listenLoop holds the LISTEN connection, reconnecting as needed. On every
// (re)connect it wakes followers, for what they may have missed.
func (s *Server) listenLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("events: listen", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) listen(ctx context.Context) error {
	conn, err := s.db.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN lux_events"); err != nil {
		return err
	}
	// The connection goes back to the pool listening: unlisten first.
	defer conn.Exec(context.Background(), "UNLISTEN lux_events")
	s.wakeups.notify()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		s.wakeups.notify()
	}
}
