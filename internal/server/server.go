// Package server is luxd: the REST API, the runner hub, the scheduler and
// the reapers.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/store"
)

type Config struct {
	Listen    string
	PublicURL string
	// LeaseDuration is how long a host may go without a heartbeat before it
	// and its live Runs are lost.
	LeaseDuration time.Duration
	// Tick is the scheduler and reaper interval.
	Tick time.Duration
	// RetentionUnit is what one day of a tenant's retention_days means.
	// 24h in production; tests shorten it to see retention happen.
	RetentionUnit time.Duration
	// Provisioners by pool provider (ec2).
	Providers map[string]Provider
}

type Server struct {
	cfg   Config
	db    *store.Store
	blobs *blob.Store
	log   *slog.Logger
	hub   *Hub
	// secrets holds submitted secret values in memory, by run id, until
	// the placement that needs them has been assigned. Never persisted.
	secrets *secretCache
	kick    chan struct{}
	wg      sync.WaitGroup
}

func New(cfg Config, db *store.Store, blobs *blob.Store, log *slog.Logger) *Server {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.Tick == 0 {
		cfg.Tick = time.Second
	}
	if cfg.RetentionUnit == 0 {
		cfg.RetentionUnit = 24 * time.Hour
	}
	s := &Server{
		cfg:     cfg,
		db:      db,
		blobs:   blobs,
		log:     log,
		secrets: newSecretCache(),
		kick:    make(chan struct{}, 1),
	}
	s.hub = newHub(s)
	return s
}

// Kick asks the scheduler to run now rather than at the next tick.
func (s *Server) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.routes(mux)
	s.runnerRoutes(mux)
	return logMiddleware(s.log, mux)
}

// Run starts the background loops and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.schedulerLoop(ctx) }()
	go func() { defer s.wg.Done(); s.reaperLoop(ctx) }()
	go func() { defer s.wg.Done(); s.hub.deliveryLoop(ctx) }()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("luxd listening", "addr", s.cfg.Listen)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.hub.closeAll()
		_ = srv.Shutdown(shutdown)
		s.wg.Wait()
		return nil
	case err := <-errc:
		return err
	}
}

type secretCache struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func newSecretCache() *secretCache { return &secretCache{m: map[string]map[string]string{}} }

func (c *secretCache) put(runID string, v map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[runID] = v
}

func (c *secretCache) get(runID string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[runID]
	return v, ok
}

func (c *secretCache) drop(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, runID)
}
