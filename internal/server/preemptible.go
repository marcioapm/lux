package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
)

// Error codes of non-preemptible Runs and pools.
const (
	codePoolRequired       = "pool_required"
	codePoolPreemptible    = "pool_preemptible"
	codePoolHasNonPreempt  = "pool_has_non_preemptible_runs"
	nonPreemptibleRunSQL   = `coalesce(r.spec->'placement'->>'preemptible', '') = 'false'`
	errPreemptibleTemplate = "template.spot %v contradicts preemptible %v: an ec2 pool's market is preemptible (spot: true is preemptible: true); leave template.spot out"
)

// PoolPreemptible is the preemptible a pool set stores: explicit, else an
// ec2 template's spot when it has one, else current (the stored value, on
// update), else true. An ec2 template spot contradicting explicit is a 422.
func PoolPreemptible(provider string, template map[string]any, explicit, current *bool) (bool, error) {
	raw, hasSpot := template["spot"]
	if hasSpot && provider == "ec2" {
		spot, ok := raw.(bool)
		if !ok {
			return false, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.spot must be true or false, got %T", raw)
		}
		if explicit != nil && *explicit != spot {
			return false, errf(http.StatusUnprocessableEntity, "invalid_pool", errPreemptibleTemplate, spot, *explicit)
		}
		return spot, nil
	}
	switch {
	case explicit != nil:
		return *explicit, nil
	case current != nil:
		return *current, nil
	}
	return true, nil
}

// ProviderTemplate is the template a pool's provider launches and checks
// with. The pool's preemptible is the one source of an ec2 pool's market:
// luxd sets the template's spot from it here, before every Launch and
// Check, so the ec2 provider reads spot as it always has. The stored
// template (and hosts.launch_template) stay as the operator wrote them.
// Other providers' templates are returned as they are.
func ProviderTemplate(provider string, template json.RawMessage, preemptible bool) (json.RawMessage, error) {
	if provider != "ec2" {
		return template, nil
	}
	var t map[string]any
	if len(template) > 0 {
		if err := json.Unmarshal(template, &t); err != nil {
			return nil, err
		}
	}
	if spot, _ := t["spot"].(bool); spot == preemptible {
		return template, nil
	}
	t = maps.Clone(t)
	if t == nil {
		t = map[string]any{}
	}
	if preemptible {
		t["spot"] = true
	} else {
		delete(t, "spot")
	}
	return json.Marshal(t)
}

// checkPreemptibleFlip refuses (409) making pool poolID preemptible while
// a non-preemptible Run bound to it could still be placed there: queued,
// live, or resumable (every state but terminated). current is its stored
// preemptible; nothing is checked unless it goes from false to true.
func checkPreemptibleFlip(ctx context.Context, tx pgx.Tx, poolID string, current, next bool) error {
	if current || !next {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r WHERE r.pool_id = $1 AND r.state <> 'terminated' AND `+nonPreemptibleRunSQL,
		poolID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errf(http.StatusConflict, codePoolHasNonPreempt,
			"%d %s with placement.preemptible false %s in this pool (queued, live or resumable): terminate or move them before making it preemptible",
			n, plural(n, "Run", "Runs"), plural(n, "is", "are"))
	}
	return nil
}

// storedPoolPreemptible is the stored preemptible of the live or retired
// pool name of tenantID ("" the platform's); nil when there is none.
func storedPoolPreemptible(ctx context.Context, tx pgx.Tx, tenantID, name string) (id string, preemptible *bool, err error) {
	var p bool
	err = tx.QueryRow(ctx, `SELECT id, preemptible FROM pools WHERE tenant_id IS NOT DISTINCT FROM nullif($1, '') AND name = $2`,
		tenantID, name).Scan(&id, &p)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return id, &p, err
}

// SavePoolPreemptible resolves a pool set's preemptible against the stored
// pool tenantID/name and refuses a false-to-true change while it holds
// non-preemptible Runs. Call it in the set's transaction, under ChangePool's
// lock (SavePool's upsert).
func SavePoolPreemptible(ctx context.Context, tx pgx.Tx, tenantID, name, provider string, template map[string]any, explicit *bool) (bool, error) {
	id, current, err := storedPoolPreemptible(ctx, tx, tenantID, name)
	if err != nil {
		return false, err
	}
	next, err := PoolPreemptible(provider, template, explicit, current)
	if err != nil || current == nil {
		return next, err
	}
	return next, checkPreemptibleFlip(ctx, tx, id, *current, next)
}

// checkNonPreemptiblePool holds a non-preemptible Run's pool to its rules:
// named explicitly (pool or poolId, not a default), and preemptible false.
// rp is the resolved pool; named reports whether the spec named it.
func checkNonPreemptiblePool(ctx context.Context, tx pgx.Tx, sp *spec.RunSpec, rp resolvedPool, named bool) error {
	if !sp.NonPreemptible() {
		return nil
	}
	if !named {
		return errf(http.StatusUnprocessableEntity, codePoolRequired,
			"placement.preemptible false needs a pool named by placement.pool or placement.poolId: default pools are not used")
	}
	return requireNonPreemptiblePool(ctx, tx, rp)
}

// requireNonPreemptiblePool refuses a pool that is preemptible, or not
// there (a name no active pool has: a Run bound to none could later bind
// to any pool of that name).
func requireNonPreemptiblePool(ctx context.Context, tx pgx.Tx, rp resolvedPool) error {
	if rp.ID == nil {
		return errf(http.StatusUnprocessableEntity, codePoolPreemptible,
			"no pool %s: placement.preemptible false needs an existing pool with preemptible false", rp.Name)
	}
	var preemptible bool
	if err := tx.QueryRow(ctx, `SELECT preemptible FROM pools WHERE id = $1`, *rp.ID).Scan(&preemptible); err != nil {
		return err
	}
	if preemptible {
		return errf(http.StatusUnprocessableEntity, codePoolPreemptible,
			"pool %s is preemptible: placement.preemptible false needs a pool with preemptible false", rp.Name)
	}
	return nil
}
