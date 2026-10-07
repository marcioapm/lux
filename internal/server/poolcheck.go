package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// maxCheckError bounds the provider's error in a refused pool's message.
const maxCheckError = 300

// CheckPoolTemplate asks the provider whether the pool tenantID/name ("" a
// platform pool) would launch with template, before it is stored: a 422
// invalid_pool "template: <provider> cannot launch it: <error>" when not. A
// provider without a Check (static) is not asked, nor is one when the live
// pool already has this provider and an identical template, so re-setting
// an unchanged pool does not need the provider to be reachable.
//
// The check carries a launch's tags (LaunchTags): this deployment, the
// pool's name, and its id: the stored one, or for a pool the set creates,
// newID, the id its insert will use. The host's Name and lux:host are a
// fresh host id's, the shape a launch gives them.
func CheckPoolTemplate(ctx context.Context, db *store.Store, providers map[string]Provider, tenantID, name, newID, provider string, template map[string]any) error {
	checker, ok := providers[provider].(Checker)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return err
	}
	stored, err := readCheckedPool(ctx, db, tenantID, name)
	if err != nil {
		return err
	}
	if stored.live && stored.provider == provider {
		same, err := sameTemplate(stored.template, raw)
		if err != nil || same {
			return err
		}
	}
	poolID := newID
	if stored.id != "" {
		poolID = stored.id
	}
	tags := LaunchTags(stored.deployment, poolID, name, ids.New(ids.Host))
	if err := checker.Check(ctx, raw, tags); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "template: %s cannot launch it: %s",
			provider, truncate(providerErrorText(err), maxCheckError))
	}
	return nil
}

// checkedPool is what CheckPoolTemplate reads: the deployment id, and the
// pool row of that name, retired or not (a set reviving a retired pool
// keeps its id); id "" when there is none.
type checkedPool struct {
	deployment, id, provider string
	live                     bool
	template                 map[string]any
}

func readCheckedPool(ctx context.Context, db *store.Store, tenantID, name string) (checkedPool, error) {
	var out checkedPool
	err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&out.deployment); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id, provider, template, NOT retired FROM pools
			WHERE tenant_id IS NOT DISTINCT FROM nullif($1, '') AND name = $2`, tenantID, name).Scan(&out.id, &out.provider, &out.template, &out.live)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}

// sameTemplate: stored and raw are equal as JSON values (key order and
// number spelling aside).
func sameTemplate(stored map[string]any, raw []byte) (bool, error) {
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		return false, err
	}
	if want == nil {
		want = map[string]any{}
	}
	if stored == nil {
		stored = map[string]any{}
	}
	return reflect.DeepEqual(stored, want), nil
}
