package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Personal keys and service accounts' keys (docs/security.md).

const (
	maxPersonalKeys = 5
	maxAccountKeys  = 2
)

// ApiKey is a key as listed: never its secret.
type ApiKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix" doc:"The first characters of the key (lux_u_ for a person's, lux_s_ for a service account's), to tell keys apart."`
	ReadOnly   bool       `json:"readOnly"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	CreatedBy  *Person    `json:"createdBy,omitempty"`
}

// NewApiKey is a key just made, with its secret: shown this once.
type NewApiKey struct {
	ApiKey
	Key string `json:"apiKey" doc:"The key. Shown only now: luxd keeps its hash."`
}

type CreateKeyInput struct {
	Name      string     `json:"name" required:"true" doc:"What the key is for (1 to 100 characters)."`
	ReadOnly  bool       `json:"readOnly,omitempty" doc:"The key reads only, whatever the role. Otherwise it has the person's role, and follows it when it changes."`
	ExpiresAt *time.Time `json:"expiresAt,omitempty" doc:"When the key stops working. None: it works until revoked."`
}

type keyList struct {
	Keys []ApiKey `json:"keys"`
}

type keysOutput struct{ Body keyList }

type keyOutput struct{ Body ApiKey }

type newKeyOutput struct {
	Status int
	Body   NewApiKey
}

type createKeyBody struct {
	Body CreateKeyInput
}

type KeyPath struct {
	Key string `path:"key" doc:"The key's id, or its prefix."`
}

type renameKeyInput struct {
	KeyPath
	Body struct {
		Name string `json:"name" required:"true"`
	}
}

func (s *Server) keyRoutes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "listMyKeys", Method: http.MethodGet, Path: "/v1/me/keys", Tags: []string{"people"},
		Summary: "Your API keys in this tenant", Description: "Active ones (neither revoked nor expired), newest first. Never their secrets.",
	}, "read", forPerson(s.listMyKeys))
	register(s, api, huma.Operation{
		OperationID: "createMyKey", Method: http.MethodPost, Path: "/v1/me/keys", Tags: []string{"people"},
		Summary: "Make yourself an API key",
		Description: "A personal key in this tenant: it acts as you, with your role (it follows role changes) or, readOnly, as a viewer. " +
			fmt.Sprintf("The secret (lux_u_...) is in the answer only. At most %d active keys per person per tenant: 409 key_limit.", maxPersonalKeys),
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, "read", forPerson(s.createMyKey))
	register(s, api, huma.Operation{
		OperationID: "renameMyKey", Method: http.MethodPatch, Path: "/v1/me/keys/{key}", Tags: []string{"people"},
		Summary: "Rename one of your API keys", Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "read", forPerson(s.renameMyKey))
	register(s, api, huma.Operation{
		OperationID: "revokeMyKey", Method: http.MethodDelete, Path: "/v1/me/keys/{key}", Tags: []string{"people"},
		Summary: "Revoke one of your API keys", Description: "It stops working at once.",
		DefaultStatus: http.StatusNoContent, Errors: []int{http.StatusNotFound},
	}, "read", forPerson(s.revokeMyKey))
}

// forPerson refuses a caller that is no person in a tenant.
func forPerson[I, O any](h func(context.Context, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		p := principal(ctx)
		if p.UserID == "" {
			return nil, errNoPerson()
		}
		if p.TenantID == "" {
			return nil, errf(http.StatusBadRequest, "tenant_required", "keys belong to a tenant: name one with ?tenant=<id or name>")
		}
		return h(ctx, in)
	}
}

// keyColumns: Select keyColumns FROM api_keys k.
const keyColumns = `k.id, k.name, k.prefix, k.read_only, k.created_at, k.last_used_at, k.expires_at, k.created_by`

const activeKey = `k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())`

func scanKeys(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]ApiKey, error) {
	rows, err := tx.Query(ctx, `SELECT `+keyColumns+` FROM api_keys k WHERE `+activeKey+` AND `+where+` ORDER BY k.created_at DESC, k.id`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		k  ApiKey
		by *string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.k.ID, &x.k.Name, &x.k.Prefix, &x.k.ReadOnly, &x.k.CreatedAt, &x.k.LastUsedAt, &x.k.ExpiresAt, &x.by)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]ApiKey, 0, len(list))
	for _, x := range list {
		if x.k.CreatedBy, err = personByID(ctx, tx, x.by); err != nil {
			return nil, err
		}
		out = append(out, x.k)
	}
	return out, nil
}

func checkKeyName(n string) error {
	if n = strings.TrimSpace(n); n == "" || len(n) > 100 {
		return errf(http.StatusUnprocessableEntity, "invalid_request", "name: 1 to 100 characters")
	}
	return nil
}

func (s *Server) listMyKeys(ctx context.Context, _ *struct{}) (*keysOutput, error) {
	p := principal(ctx)
	var keys []ApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		keys, err = scanKeys(ctx, tx, `k.user_id = $1 AND k.tenant_id = $2`, p.UserID, p.TenantID)
		return err
	})
	return &keysOutput{keyList{keys}}, err
}

// keySpec is a key to make: whose (column = id), its prefix and cap.
type keySpec struct {
	col, owner, prefix string
	limit              int
	readOnly           bool
	expires            *time.Time
	name               string
}

// insertKey makes a key under the cap. The cap holds against concurrent
// creates: they take one transaction-scoped advisory lock per owner and
// tenant before counting, so the count and the insert are one step.
func insertKey(ctx context.Context, tx pgx.Tx, p Principal, k keySpec) (NewApiKey, error) {
	var out NewApiKey
	if p.ReadOnly {
		return out, errf(403, "forbidden", "this key is read-only")
	}
	if err := checkKeyName(k.name); err != nil {
		return out, err
	}
	if k.expires != nil && !k.expires.After(time.Now()) {
		return out, errf(http.StatusUnprocessableEntity, "invalid_request", "expiresAt must be in the future")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('lux_keys:' || $1 || ':' || $2))`, p.TenantID, k.owner); err != nil {
		return out, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys k WHERE `+activeKey+` AND k.tenant_id = $1 AND k.`+k.col+` = $2`, p.TenantID, k.owner).Scan(&n); err != nil {
		return out, err
	}
	if n >= k.limit {
		return out, errf(http.StatusConflict, "key_limit", "at most %d active keys: revoke one first", k.limit)
	}
	secret := ids.Secret(k.prefix)
	out.Key = secret
	out.ID, out.Name, out.ReadOnly, out.ExpiresAt = ids.New(ids.APIKey), strings.TrimSpace(k.name), k.readOnly, k.expires
	out.Prefix = secret[:len(k.prefix)+5]
	var by *string
	if p.UserID != "" {
		by = &p.UserID
	}
	err := tx.QueryRow(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, `+k.col+`, read_only, expires_at, created_by, prefix)
		VALUES ($1, $2, $3, $4, '{}', $5, $6, $7, $8, $9) RETURNING created_at`,
		out.ID, p.TenantID, out.Name, ids.Hash(secret), k.owner, k.readOnly, k.expires, by, out.Prefix).Scan(&out.CreatedAt)
	if err != nil {
		return out, err
	}
	out.CreatedBy, err = personByID(ctx, tx, by)
	return out, err
}

func (s *Server) createMyKey(ctx context.Context, in *createKeyBody) (*newKeyOutput, error) {
	p := principal(ctx)
	var out NewApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		out, err = insertKey(ctx, tx, p, keySpec{col: "user_id", owner: p.UserID, prefix: "lux_u", limit: maxPersonalKeys,
			readOnly: in.Body.ReadOnly, expires: in.Body.ExpiresAt, name: in.Body.Name})
		if err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, "key.created", p, map[string]any{"key": out.ID, "name": out.Name, "user": p.UserID, "readOnly": out.ReadOnly})
	})
	if err != nil {
		return nil, err
	}
	return &newKeyOutput{http.StatusCreated, out}, nil
}

// findKey resolves a key by id or prefix among where's.
func findKey(ctx context.Context, tx pgx.Tx, ref, where string, args ...any) (string, error) {
	args = append(args, ref)
	n := "$" + strconv.Itoa(len(args))
	var id string
	err := tx.QueryRow(ctx, `SELECT k.id FROM api_keys k WHERE `+activeKey+` AND `+where+` AND (k.id = `+n+` OR (k.prefix <> '' AND k.prefix = `+n+`))
		ORDER BY k.id = `+n+` DESC LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusNotFound, "not_found", "no active key %s", ref)
	}
	return id, err
}

func (s *Server) renameMyKey(ctx context.Context, in *renameKeyInput) (*keyOutput, error) {
	p := principal(ctx)
	if p.ReadOnly {
		return nil, errf(403, "forbidden", "this key is read-only")
	}
	if err := checkKeyName(in.Body.Name); err != nil {
		return nil, err
	}
	var out []ApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := findKey(ctx, tx, in.Key, `k.user_id = $1 AND k.tenant_id = $2`, p.UserID, p.TenantID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET name = $2 WHERE id = $1`, id, strings.TrimSpace(in.Body.Name)); err != nil {
			return err
		}
		out, err = scanKeys(ctx, tx, `k.id = $1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &keyOutput{out[0]}, nil
}

func (s *Server) revokeMyKey(ctx context.Context, in *KeyPath) (*struct{}, error) {
	p := principal(ctx)
	if p.ReadOnly {
		return nil, errf(403, "forbidden", "this key is read-only")
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := findKey(ctx, tx, in.Key, `k.user_id = $1 AND k.tenant_id = $2`, p.UserID, p.TenantID)
		if err != nil {
			return err
		}
		return revokeKey(ctx, tx, p, id)
	})
	return nil, err
}

func revokeKey(ctx context.Context, tx pgx.Tx, p Principal, id string) error {
	var name string
	if err := tx.QueryRow(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING name`, id).Scan(&name); err != nil {
		return err
	}
	return addTenantEvent(ctx, tx, p.TenantID, "key.revoked", p, map[string]any{"key": id, "name": name})
}

// dnsLabel: a service account's name.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
