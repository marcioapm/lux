package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

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
func CheckPoolTemplate(ctx context.Context, db *store.Store, providers map[string]Provider, tenantID, name, provider string, template map[string]any) error {
	checker, ok := providers[provider].(Checker)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return err
	}
	same, err := storedTemplateIs(ctx, db, tenantID, name, provider, raw)
	if err != nil || same {
		return err
	}
	if err := checker.Check(ctx, raw); err != nil {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "template: %s cannot launch it: %s",
			provider, truncate(providerErrorText(err), maxCheckError))
	}
	return nil
}

// storedTemplateIs: the live pool tenantID/name has provider and a template
// equal to raw, compared as JSON values (key order and number spelling
// aside).
func storedTemplateIs(ctx context.Context, db *store.Store, tenantID, name, provider string, raw []byte) (bool, error) {
	var storedProvider string
	var stored map[string]any
	err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider, template FROM pools
			WHERE tenant_id IS NOT DISTINCT FROM nullif($1, '') AND name = $2 AND NOT retired`, tenantID, name).Scan(&storedProvider, &stored)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || storedProvider != provider {
		return false, err
	}
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
