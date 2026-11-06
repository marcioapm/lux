package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// addTenantEvent records an event of the tenant's people on its feed:
// data gains by, the actor. subject is what it is about (data's first
// id-like value).
func addTenantEvent(ctx context.Context, tx pgx.Tx, tenantID, typ string, p Principal, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["by"] = p.Actor()
	subject := ""
	for _, k := range []string{"user", "serviceAccount", "invitation", "key"} {
		if v, ok := data[k].(string); ok && v != "" {
			subject = v
			break
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO run_events (tenant_id, type, data, subject) VALUES ($1, $2, $3, $4)`, tenantID, typ, data, subject)
	return err
}

// ServiceAccount is a tenant's non-person principal: its keys act with its
// role.
type ServiceAccount struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Role        string     `json:"role" enum:"viewer,member,admin"`
	Owner       *Person    `json:"owner,omitempty" doc:"The person who may rotate its keys and act on its Runs (as a member); absent: none."`
	CreatedAt   time.Time  `json:"createdAt"`
	CreatedBy   *Person    `json:"createdBy,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty" doc:"The latest use of any of its keys (to the minute)."`
	DisabledAt  *time.Time `json:"disabledAt,omitempty" doc:"Disabled: its keys work no more."`
	ActiveKeys  int        `json:"activeKeys"`
}

type SAPath struct {
	SA string `path:"sa" doc:"The service account's id or name."`
}

type SAKeyPath struct {
	SAPath
	Key string `path:"key" doc:"The key's id, or its prefix."`
}

type CreateServiceAccountInput struct {
	Name        string `json:"name" required:"true" doc:"Lowercase letters, digits and dashes, starting and ending with a letter or digit, up to 63; unique in the tenant."`
	Description string `json:"description,omitempty"`
	Role        string `json:"role" required:"true" enum:"viewer,member,admin"`
	OwnerEmail  string `json:"ownerEmail,omitempty" doc:"Its owner: a member of the tenant, by email."`
}

type PatchServiceAccountInput struct {
	Description *string `json:"description,omitempty"`
	Role        *string `json:"role,omitempty" enum:"viewer,member,admin"`
	OwnerEmail  *string `json:"ownerEmail,omitempty" doc:"A member's email, or \"\" for no owner."`
	Disabled    *bool   `json:"disabled,omitempty" doc:"true disables it (its keys stop working); false enables it again."`
}

type saOutput struct {
	Status int
	Body   ServiceAccount
}

type saList struct {
	ServiceAccounts []ServiceAccount `json:"serviceAccounts"`
}

type saListOutput struct{ Body saList }

func (s *Server) serviceAccountRoutes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "listServiceAccounts", Method: http.MethodGet, Path: "/v1/service-accounts", Tags: []string{"people"},
		Summary: "List the tenant's service accounts", Description: "By name, disabled ones included.",
	}, "read", forTenant(s.listServiceAccounts))
	register(s, api, huma.Operation{
		OperationID: "createServiceAccount", Method: http.MethodPost, Path: "/v1/service-accounts", Tags: []string{"people"},
		Summary: "Create a service account",
		Description: "A principal of the tenant that is no person (a bot, CI, an orchestrator): its keys act with its role, viewer, member or admin (never owner). " +
			"Admins and owners only. An owner, if given, must be a member of the tenant: they may rotate its keys and act on its Runs, not change its role. " +
			"409 conflict for a name taken in the tenant.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, "admin", forTenant(s.createServiceAccount))
	register(s, api, huma.Operation{
		OperationID: "getServiceAccount", Method: http.MethodGet, Path: "/v1/service-accounts/{sa}", Tags: []string{"people"},
		Summary: "A service account", Errors: []int{http.StatusNotFound},
	}, "read", forTenant(s.getServiceAccount))
	register(s, api, huma.Operation{
		OperationID: "patchServiceAccount", Method: http.MethodPatch, Path: "/v1/service-accounts/{sa}", Tags: []string{"people"},
		Summary:     "Change a service account",
		Description: "Its description, role, owner, or disabled. Admins and owners only. Disabling stops its keys at once; its Runs keep running.",
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "admin", forTenant(s.patchServiceAccount))
	register(s, api, huma.Operation{
		OperationID: "listServiceAccountKeys", Method: http.MethodGet, Path: "/v1/service-accounts/{sa}/keys", Tags: []string{"people"},
		Summary: "A service account's API keys", Description: "Its active keys. Its owner and admins only (others: 404).",
		Errors: []int{http.StatusNotFound},
	}, "read", forTenant(s.listServiceAccountKeys))
	register(s, api, huma.Operation{
		OperationID: "createServiceAccountKey", Method: http.MethodPost, Path: "/v1/service-accounts/{sa}/keys", Tags: []string{"people"},
		Summary:       "Make a service account a key",
		Description:   fmt.Sprintf("Its owner and admins only. The secret (lux_s_...) is in the answer only. At most %d active keys: 409 key_limit (rotate: make the new key, move to it, revoke the old one).", maxAccountKeys),
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "read", forTenant(s.createServiceAccountKey))
	register(s, api, huma.Operation{
		OperationID: "revokeServiceAccountKey", Method: http.MethodDelete, Path: "/v1/service-accounts/{sa}/keys/{key}", Tags: []string{"people"},
		Summary: "Revoke a service account's key", Description: "Its owner and admins only.",
		DefaultStatus: http.StatusNoContent, Errors: []int{http.StatusNotFound},
	}, "read", forTenant(s.revokeServiceAccountKey))
}

const saColumns = `sa.id, sa.name, sa.description, sa.role, sa.owner_user_id, sa.created_at, sa.created_by, sa.disabled_at,
	(SELECT max(k.last_used_at) FROM api_keys k WHERE k.service_account_id = sa.id),
	(SELECT count(*) FROM api_keys k WHERE k.service_account_id = sa.id AND ` + activeKey + `)::int`

func scanAccounts(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]ServiceAccount, error) {
	rows, err := tx.Query(ctx, `SELECT `+saColumns+` FROM service_accounts sa WHERE `+where+` ORDER BY sa.name`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		a         ServiceAccount
		owner, by *string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.a.ID, &x.a.Name, &x.a.Description, &x.a.Role, &x.owner, &x.a.CreatedAt, &x.by, &x.a.DisabledAt, &x.a.LastUsedAt, &x.a.ActiveKeys)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	ids := make([]*string, 0, 2*len(list))
	for _, x := range list {
		ids = append(ids, x.owner, x.by)
	}
	people, err := peopleByIDs(ctx, tx, ids...)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceAccount, 0, len(list))
	for _, x := range list {
		x.a.Owner, x.a.CreatedBy = person(people, x.owner), person(people, x.by)
		out = append(out, x.a)
	}
	return out, nil
}

// loadAccount finds a service account of the tenant by id or name.
func loadAccount(ctx context.Context, tx pgx.Tx, tenant, ref string) (ServiceAccount, string, error) {
	list, err := scanAccounts(ctx, tx, `sa.tenant_id = $1 AND (sa.id = $2 OR sa.name = $2)`, tenant, ref)
	if err != nil {
		return ServiceAccount{}, "", err
	}
	if len(list) == 0 {
		return ServiceAccount{}, "", errf(http.StatusNotFound, "not_found", "no service account %s", ref)
	}
	owner := ""
	if list[0].Owner != nil {
		owner = list[0].Owner.ID
	}
	return list[0], owner, nil
}

// memberByEmail is the id of a member of the tenant by email.
func memberByEmail(ctx context.Context, tx pgx.Tx, tenant, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT u.id FROM users u JOIN memberships m ON m.user_id = u.id AND m.tenant_id = $1 WHERE lower(u.email) = lower($2)`, tenant, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusUnprocessableEntity, "invalid_request", "%s is not a member of this tenant", email)
	}
	return id, err
}

func (s *Server) listServiceAccounts(ctx context.Context, _ *struct{}) (*saListOutput, error) {
	p := principal(ctx)
	var out []ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		out, err = scanAccounts(ctx, tx, `sa.tenant_id = $1`, p.TenantID)
		return err
	})
	return &saListOutput{saList{out}}, err
}

func (s *Server) getServiceAccount(ctx context.Context, in *SAPath) (*saOutput, error) {
	p := principal(ctx)
	var out ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		out, _, err = loadAccount(ctx, tx, p.TenantID, in.SA)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &saOutput{http.StatusOK, out}, nil
}

func (s *Server) createServiceAccount(ctx context.Context, in *struct{ Body CreateServiceAccountInput }) (*saOutput, error) {
	p := principal(ctx)
	b := in.Body
	if !dnsLabel.MatchString(b.Name) {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_request", "name %q: lowercase letters, digits and dashes, starting and ending with a letter or digit, at most 63", b.Name)
	}
	if b.Role != "viewer" && b.Role != "member" && b.Role != "admin" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_request", "role: viewer, member or admin")
	}
	var out ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var owner *string
		if b.OwnerEmail != "" {
			id, err := memberByEmail(ctx, tx, p.TenantID, b.OwnerEmail)
			if err != nil {
				return err
			}
			owner = &id
		}
		var by *string
		if p.UserID != "" {
			by = &p.UserID
		}
		id := ids.New(ids.ServiceAccount)
		tag, err := tx.Exec(ctx, `INSERT INTO service_accounts (id, tenant_id, name, description, role, owner_user_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (tenant_id, name) DO NOTHING`, id, p.TenantID, b.Name, b.Description, b.Role, owner, by)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(http.StatusConflict, "conflict", "a service account %s exists in this tenant", b.Name)
		}
		if out, _, err = loadAccount(ctx, tx, p.TenantID, id); err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, "service_account.created", p, map[string]any{"serviceAccount": id, "name": b.Name, "role": b.Role})
	})
	if err != nil {
		return nil, err
	}
	return &saOutput{http.StatusCreated, out}, nil
}

func (s *Server) patchServiceAccount(ctx context.Context, in *struct {
	SAPath
	Body PatchServiceAccountInput
}) (*saOutput, error) {
	p := principal(ctx)
	b := in.Body
	if b.Role != nil && *b.Role != "viewer" && *b.Role != "member" && *b.Role != "admin" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_request", "role: viewer, member or admin")
	}
	var out ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		sa, _, err := loadAccount(ctx, tx, p.TenantID, in.SA)
		if err != nil {
			return err
		}
		changed := map[string]any{"serviceAccount": sa.ID, "name": sa.Name}
		if b.Description != nil {
			if _, err := tx.Exec(ctx, `UPDATE service_accounts SET description = $2 WHERE id = $1`, sa.ID, *b.Description); err != nil {
				return err
			}
			changed["description"] = *b.Description
		}
		if b.Role != nil {
			if _, err := tx.Exec(ctx, `UPDATE service_accounts SET role = $2 WHERE id = $1`, sa.ID, *b.Role); err != nil {
				return err
			}
			changed["role"] = *b.Role
		}
		if b.OwnerEmail != nil {
			var owner *string
			if *b.OwnerEmail != "" {
				id, err := memberByEmail(ctx, tx, p.TenantID, *b.OwnerEmail)
				if err != nil {
					return err
				}
				owner = &id
			}
			if _, err := tx.Exec(ctx, `UPDATE service_accounts SET owner_user_id = $2 WHERE id = $1`, sa.ID, owner); err != nil {
				return err
			}
			changed["owner"] = *b.OwnerEmail
		}
		typ := "service_account.updated"
		if b.Disabled != nil {
			if _, err := tx.Exec(ctx, `UPDATE service_accounts SET disabled_at = CASE WHEN $2 THEN coalesce(disabled_at, now()) END WHERE id = $1`, sa.ID, *b.Disabled); err != nil {
				return err
			}
			changed["disabled"] = *b.Disabled
			if *b.Disabled && sa.DisabledAt == nil {
				typ = "service_account.disabled"
			}
		}
		if out, _, err = loadAccount(ctx, tx, p.TenantID, sa.ID); err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, typ, p, changed)
	})
	if err != nil {
		return nil, err
	}
	return &saOutput{http.StatusOK, out}, nil
}

// accountKeyHolder loads the account for a key route: its owner and
// admins may; anyone else is told it is not there.
func accountKeyHolder(ctx context.Context, tx pgx.Tx, p Principal, ref string) (ServiceAccount, error) {
	sa, owner, err := loadAccount(ctx, tx, p.TenantID, ref)
	if err != nil {
		return sa, err
	}
	if p.atLeast("admin") || (owner != "" && owner == p.UserID && !p.ReadOnly) {
		return sa, nil
	}
	return sa, errf(http.StatusNotFound, "not_found", "no service account %s", ref)
}

func (s *Server) listServiceAccountKeys(ctx context.Context, in *SAPath) (*keysOutput, error) {
	p := principal(ctx)
	var keys []ApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		sa, err := accountKeyHolder(ctx, tx, p, in.SA)
		if err != nil {
			return err
		}
		keys, err = scanKeys(ctx, tx, `k.service_account_id = $1`, sa.ID)
		return err
	})
	return &keysOutput{keyList{keys}}, err
}

func (s *Server) createServiceAccountKey(ctx context.Context, in *struct {
	SAPath
	Body CreateKeyInput
}) (*newKeyOutput, error) {
	p := principal(ctx)
	var out NewApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		sa, err := accountKeyHolder(ctx, tx, p, in.SA)
		if err != nil {
			return err
		}
		out, err = insertKey(ctx, tx, p, keySpec{col: "service_account_id", owner: sa.ID, prefix: "lux_s", limit: maxAccountKeys,
			readOnly: in.Body.ReadOnly, expires: in.Body.ExpiresAt, name: in.Body.Name})
		if err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, "key.created", p, map[string]any{"key": out.ID, "name": out.Name, "serviceAccount": sa.ID})
	})
	if err != nil {
		return nil, err
	}
	return &newKeyOutput{http.StatusCreated, out}, nil
}

func (s *Server) revokeServiceAccountKey(ctx context.Context, in *SAKeyPath) (*struct{}, error) {
	p := principal(ctx)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		sa, err := accountKeyHolder(ctx, tx, p, in.SA)
		if err != nil {
			return err
		}
		id, err := findKey(ctx, tx, in.Key, `k.service_account_id = $1`, sa.ID)
		if err != nil {
			return err
		}
		return revokeKey(ctx, tx, p, id)
	})
	return nil, err
}
