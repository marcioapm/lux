package server

import (
	"encoding/json"
	"time"
)

// A pool whose launch fails launches nothing again until its backoff
// expires: launchBackoffInitial after the first failure, doubling per
// consecutive failure, capped at launchBackoffMax. Held in memory by the
// luxd holding the provisioner lease, so a restart, or another luxd taking
// the lease over, starts every pool afresh.
const (
	launchBackoffInitial = 15 * time.Second
	launchBackoffMax     = 5 * time.Minute
)

type poolBackoff struct {
	failures  int
	notBefore time.Time
	lastErr   error
	// config is the pool row the failures happened under (poolConfig).
	config string
}

// launchRefused wraps the provider's own error from launch, so the pass
// tells it apart from a database error or a quota refusal.
type launchRefused struct{ err error }

func (e *launchRefused) Error() string { return e.err.Error() }
func (e *launchRefused) Unwrap() error { return e.err }

// poolConfig fingerprints a pool's stored settings: any change to them
// (template, min/max/warm, provider, ...) ends its backoff.
func poolConfig(pl poolRow) string {
	b, _ := json.Marshal(pl)
	return string(b)
}

// launchBackoffFor is the pool's backoff, or nil when it has none or its
// settings changed since its failures (then forgotten).
func (s *Server) launchBackoffFor(pl poolRow) *poolBackoff {
	bo := s.launchBackoff[pl.ID]
	if bo != nil && bo.config != poolConfig(pl) {
		delete(s.launchBackoff, pl.ID)
		return nil
	}
	return bo
}

// launchFailed counts a provider's refusal of a launch for the pool and
// sets when it may launch again.
func (s *Server) launchFailed(pl poolRow, err error) {
	if s.launchBackoff == nil {
		s.launchBackoff = map[string]*poolBackoff{}
	}
	bo := s.launchBackoffFor(pl)
	if bo == nil {
		bo = &poolBackoff{config: poolConfig(pl)}
		s.launchBackoff[pl.ID] = bo
	}
	bo.failures++
	delay := s.cfg.LaunchBackoff
	for i := 1; i < bo.failures && delay < s.cfg.LaunchBackoffMax; i++ {
		delay *= 2
	}
	bo.notBefore = s.now().Add(min(delay, s.cfg.LaunchBackoffMax))
	bo.lastErr = err
}

// forgetLaunchBackoffs drops the backoffs of pools no longer provisioned.
func (s *Server) forgetLaunchBackoffs(pools []poolRow) {
	live := make(map[string]bool, len(pools))
	for _, pl := range pools {
		live[pl.ID] = true
	}
	for id := range s.launchBackoff {
		if !live[id] {
			delete(s.launchBackoff, id)
		}
	}
}
