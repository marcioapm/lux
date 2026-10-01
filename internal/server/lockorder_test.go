package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

// runThenServers holds what a runner's report of the Run's end holds when
// it ends the Run's servers (applyReport, then endServers): the Run's row,
// then its servers' rows. It takes the Run, waits for release, then takes
// the servers and commits.
func runThenServers(t *testing.T, s *Server, ctx context.Context, runID string, locked chan<- struct{}, release <-chan struct{}) error {
	t.Helper()
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
			return err
		}
		close(locked)
		<-release
		_, err := tx.Exec(ctx, `SELECT 1 FROM run_servers WHERE run_id = $1 FOR UPDATE`, runID)
		return err
	})
}

// waitBlocked returns once some other backend waits on a lock.
func waitBlocked(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		systemScan(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`, nil, &n)
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing blocked within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// Deleting a server while its Run's end is being recorded: both take the
// Run first, then the server, so one waits for the other and neither
// deadlocks. The same for expiry.
func TestServerChangesLockTheRunFirst(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, s *Server, ctx context.Context, key, id string) error
	}{
		{"delete", func(t *testing.T, s *Server, ctx context.Context, key, id string) error {
			if w := apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+id, nil); w.Code != http.StatusNoContent {
				return fmt.Errorf("%d %s", w.Code, w.Body)
			}
			return nil
		}},
		{"expiry", func(_ *testing.T, s *Server, ctx context.Context, _, _ string) error { return s.expireServers(ctx) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"lifetime": "owner", "expireAfter": "1h", "hostname": "web.lock.lux.example.com"})
			execSQL(t, s, ctx, `UPDATE run_servers SET created_at = now() - interval '2 hours' WHERE id = $1`, sv.ID)
			locked, release := make(chan struct{}), make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			report := make(chan error, 1)
			go func() { report <- runThenServers(t, s, ctx, r1, locked, release) }()
			<-locked
			change := make(chan error, 1)
			go func() { change <- c.change(t, s, ctx, key, sv.ID) }()
			waitBlocked(t, s)
			close(release)
			released = true
			rerr, cerr := <-report, <-change
			if isDeadlock(rerr) || isDeadlock(cerr) || rerr != nil || cerr != nil {
				t.Fatalf("report: %v; %s: %v", rerr, c.name, cerr)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusNotFound {
				t.Fatalf("after the %s: %d %s", c.name, w.Code, w.Body)
			}
		})
	}
}

// An expiry pass waiting for the Run's lock acts on the server as it is
// once it has the lock: one requested meanwhile is no longer due and
// stays; one detached meanwhile is left for the next pass, which expires
// it.
func TestExpiryRechecksUnderLock(t *testing.T) {
	for _, c := range []struct {
		name, meanwhile string
		nextPass        int
	}{
		{"requested meanwhile", `UPDATE run_servers SET last_request_at = now() WHERE id = $1`, http.StatusOK},
		{"detached meanwhile", `UPDATE run_servers SET run_id = NULL WHERE id = $1`, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"lifetime": "owner", "expireAfter": "1h", "hostname": "web.race.lux.example.com"})
			execSQL(t, s, ctx, `UPDATE run_servers SET created_at = now() - interval '2 hours' WHERE id = $1`, sv.ID)
			locked, release := make(chan struct{}), make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			report := make(chan error, 1)
			go func() { report <- runThenServers(t, s, ctx, r1, locked, release) }()
			<-locked
			expired := make(chan error, 1)
			go func() { expired <- s.expireServers(context.Background()) }()
			waitBlocked(t, s)
			execSQL(t, s, ctx, c.meanwhile, sv.ID)
			close(release)
			released = true
			if err := <-report; err != nil {
				t.Fatal(err)
			}
			if err := <-expired; err != nil {
				t.Fatal(err)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusOK {
				t.Fatalf("%s, yet expired: %d %s", c.name, w.Code, w.Body)
			}
			if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.expired")); n != 0 {
				t.Fatalf("server.expired events: %d", n)
			}
			if err := s.expireServers(ctx); err != nil {
				t.Fatal(err)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != c.nextPass {
				t.Fatalf("after the next pass: %d, want %d", w.Code, c.nextPass)
			}
		})
	}
}

// The idle check and a wake write a server's row and an event of its Run
// while its Run's end is being recorded: they take the Run (KEY SHARE, as
// the event's foreign key does) before the server, so neither deadlocks
// with the report, and each still writes its one event.
func TestIdleAndWakeLockTheRunFirst(t *testing.T) {
	for _, c := range []struct {
		name, event string
		setup       string
		write       func(s *Server, id string) error
	}{
		{"idle", "server.idle",
			`UPDATE run_servers SET state = 'ready', ready_since = now() - interval '5 minutes', last_request_at = now() - interval '2 minutes' WHERE id = $1`,
			func(s *Server, _ string) error { return s.checkIdle(context.Background()) }},
		{"wake", "server.wake_requested", `SELECT $1::text`,
			func(s *Server, id string) error {
				asked, err := s.requestWake(context.Background(), id, "ada@example.com", "/")
				if err == nil && !asked {
					err = errors.New("did not ask")
				}
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"wake": "request", "idleAfter": "1m"})
			execSQL(t, s, ctx, c.setup, sv.ID)
			locked, release := make(chan struct{}), make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			report := make(chan error, 1)
			go func() { report <- runThenServers(t, s, ctx, r1, locked, release) }()
			<-locked
			wrote := make(chan error, 1)
			go func() { wrote <- c.write(s, sv.ID) }()
			waitBlocked(t, s)
			close(release)
			released = true
			rerr, werr := <-report, <-wrote
			if rerr != nil || werr != nil {
				t.Fatalf("report: %v; %s: %v", rerr, c.name, werr)
			}
			if n := len(serverEventsOf(t, s, ctx, sv.ID, c.event)); n != 1 {
				t.Fatalf("%s events: %d", c.event, n)
			}
		})
	}
}
