package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// minTimeoutLeft is the least running time a timeout change must leave.
const minTimeoutLeft = time.Minute

// TimeoutRequest changes a Run's timeout: exactly one of its fields.
type TimeoutRequest struct {
	Add       *spec.Duration `json:"add,omitempty" doc:"Add this to the Run's timeout (\"1h\"). A Run without a timeout has nothing to add to: 422 invalid_spec, use set."`
	Set       *spec.Duration `json:"set,omitempty" doc:"The new timeout, the total running time over all its placements (\"4h\")."`
	Unlimited bool           `json:"unlimited,omitempty" doc:"true: no timeout from now on."`
	decodeErr error
}

// UnmarshalJSON keeps a decode error for extendRun to refuse as 422 invalid_spec.
func (in *TimeoutRequest) UnmarshalJSON(data []byte) error {
	type plain TimeoutRequest
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		in.decodeErr = err
		return nil
	}
	*in = TimeoutRequest(decoded)
	return nil
}

type timeoutRunInput struct {
	RunPath
	Body TimeoutRequest
}

// check refuses a body that does not ask for exactly one change.
func (r TimeoutRequest) check() error {
	if r.decodeErr != nil {
		return errf(http.StatusUnprocessableEntity, "invalid_spec", "invalid timeout change: %s", r.decodeErr)
	}
	n := 0
	for _, given := range []bool{r.Add != nil, r.Set != nil, r.Unlimited} {
		if given {
			n++
		}
	}
	if n != 1 {
		return errf(http.StatusUnprocessableEntity, "invalid_spec", "give exactly one of add, set or unlimited: true")
	}
	if r.Add != nil && r.Add.Duration <= 0 {
		return errf(http.StatusUnprocessableEntity, "invalid_spec", "add must be greater than 0")
	}
	if r.Set != nil && r.Set.Duration <= 0 {
		return errf(http.StatusUnprocessableEntity, "invalid_spec", "set must be greater than 0: unlimited: true clears the timeout")
	}
	return nil
}

// timeoutRun changes a Run's timeout under its row lock, so concurrent adds
// each count.
func (s *Server) timeoutRun(ctx context.Context, in *timeoutRunInput) (*runOutput, error) {
	p := principal(ctx)
	if err := in.Body.check(); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		// Lock first: a statement that waits for the lock keeps its older
		// snapshot of placements, missing a timeout stop committed meanwhile.
		if _, err := tx.Exec(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, in.ID); err != nil {
			return err
		}
		var state, stored string
		var stopReason *string
		if err := tx.QueryRow(ctx, `SELECT r.state, coalesce(r.spec->>'timeout', ''), (SELECT p.stop_reason FROM placements p
				WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.stop_requested_at IS NOT NULL AND p.state IN `+livePlacementStates+`)
			FROM runs r WHERE r.id = $1`, in.ID).Scan(&state, &stored, &stopReason); err != nil {
			return err
		}
		var from spec.Duration
		if err := from.UnmarshalJSON([]byte(strconv.Quote(stored))); err != nil {
			return err
		}
		if terminal(state) {
			return errf(http.StatusConflict, "run_terminated", "run is terminated: its timeout cannot change")
		}
		if stopReason != nil && *stopReason == "timeout" {
			return errf(http.StatusConflict, "timeout_stop_started",
				"the Run is already being stopped for its timeout, which cannot be called back: once it has failed, resume it with a longer timeout")
		}
		var to time.Duration
		switch {
		case in.Body.Unlimited:
		case in.Body.Set != nil:
			to = in.Body.Set.Duration
		case from.Duration <= 0:
			return errf(http.StatusUnprocessableEntity, "invalid_spec", "the Run has no timeout to add to: set one instead")
		default:
			to = from.Duration + in.Body.Add.Duration
		}
		return changeTimeout(ctx, tx, p, in.ID, from.Duration, to)
	})
	if err != nil {
		return nil, err
	}
	return s.getRun(ctx, &in.RunPath)
}

// changeTimeout writes to (0: no timeout) into locked Run id's spec, with a
// timeout.changed event, once to leaves minTimeoutLeft past its runtime.
// The same timeout again changes nothing.
func changeTimeout(ctx context.Context, tx pgx.Tx, p Principal, id string, from, to time.Duration) error {
	if to == from {
		return nil
	}
	if to > 0 {
		var seconds float64
		if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM `+timedRuntimeSQL+`)::float8 FROM runs r WHERE r.id = $1`, id).Scan(&seconds); err != nil {
			return err
		}
		used := time.Duration(seconds * float64(time.Second))
		if least := used + minTimeoutLeft; to < least {
			return errf(http.StatusUnprocessableEntity, "timeout_too_short",
				"timeout %s is too short: the Run has run %s, so the least it may be given is %s",
				to, used.Round(time.Second), least.Round(time.Second))
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET updated_at = now(),
			spec = CASE WHEN $2 = '' THEN spec - 'timeout' ELSE jsonb_set(spec, '{timeout}', to_jsonb($2::text)) END
		WHERE id = $1`, id, durationText(to)); err != nil {
		return err
	}
	return addEvent(ctx, tx, p.TenantID, id, 0, "timeout.changed", map[string]any{"by": p.Actor(), "from": durationText(from), "to": durationText(to)})
}

// durationText is a timeout as the spec stores it, "" for none.
func durationText(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
