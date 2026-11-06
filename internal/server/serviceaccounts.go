package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
	Runs        int        `json:"runs" doc:"The Runs it owns, of any state and age. An account that owns Runs cannot be deleted (disable it instead); converting it moves them."`
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
	Name        *string `json:"name,omitempty" doc:"A new name: as create's (a DNS label, unique in the tenant: 409 conflict). Its active keys named exactly as the old name (the upgrade named each legacy key's account after the key) are renamed too."`
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
		Summary: "Change a service account",
		Description: "Its name, description, role, owner, or disabled. Admins and owners only. Disabling stops its keys at once; its Runs keep running. " +
			"A new name is validated as create's (422 invalid_request) and must be free in the tenant (409 conflict); its active keys named exactly as the old name follow it. " +
			"Records service_account.updated (with from and to on a rename) or service_account.disabled.",
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
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
	register(s, api, huma.Operation{
		OperationID: "deleteServiceAccount", Method: http.MethodDelete, Path: "/v1/service-accounts/{sa}", Tags: []string{"people"},
		Summary: "Delete a service account",
		Description: "Admins and owners only. Only an account that owns no Runs: otherwise 409 has_runs (with the count), and the answer is to disable it, which stops its keys and keeps its Runs attributed. " +
			"Its keys stop working at once: active ones are revoked (key.revoked each) and every key row is kept, revoked, for the record, no longer tied to the account. Records service_account.deleted.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, "admin", forTenant(s.deleteServiceAccount))
	register(s, api, huma.Operation{
		OperationID: "convertServiceAccount", Method: http.MethodPost, Path: "/v1/service-accounts/{sa}/convert", Tags: []string{"people"},
		Summary: "Make a service account's keys a member's personal keys",
		Description: "For a key that was always a person's (the upgrade made every legacy key a service account's). Admins and owners only, in one step: " +
			"each active key of the account becomes the member's personal key (its secret, read-only and expiry unchanged), so it acts as that member, with their role, from the next request; " +
			"every Run the account owns becomes the member's; the account is deleted (its revoked keys kept, revoked, as on delete). " +
			fmt.Sprintf("The member must be one of the tenant (404 not_a_member). 409 key_limit if the member would have more than %d active keys (nothing moves). ", maxPersonalKeys) +
			"The key gains the member's role, not the account's: a caller converts only to a member whose role is at most their own (an owner: owners and operators only; 403 otherwise). " +
			"Records service_account.converted {serviceAccount, name, user, keys, runs}.",
		Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "admin", forTenant(s.convertServiceAccount))
}

type ConvertServiceAccountInput struct {
	Email string `json:"email" required:"true" doc:"The member whose personal keys they become, by email (or user id)."`
}

type ConvertedServiceAccount struct {
	User Member `json:"user" doc:"The member, as now."`
	Keys int    `json:"keys" doc:"Keys that became the member's."`
	Runs int    `json:"runs" doc:"Runs that became the member's."`
}

type convertOutput struct{ Body ConvertedServiceAccount }

func (s *Server) convertServiceAccount(ctx context.Context, in *struct {
	SAPath
	Body ConvertServiceAccountInput
}) (*convertOutput, error) {
	p := principal(ctx)
	if !p.atLeast("admin") {
		return nil, errf(http.StatusForbidden, "forbidden", "only admins and owners convert service accounts")
	}
	var out ConvertedServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockAccounts(ctx, tx, p.TenantID); err != nil {
			return err
		}
		sa, _, err := loadAccount(ctx, tx, p.TenantID, in.SA)
		if err != nil {
			return err
		}
		m, err := findMember(ctx, tx, p, in.Body.Email)
		if errors.Is(err, errNotFound) {
			return errf(http.StatusNotFound, "not_a_member", "%s is not a member of this tenant", in.Body.Email)
		}
		if err != nil {
			return err
		}
		// The keys will act with the member's role: never more than the caller's own.
		if !p.atLeast(m.Role) {
			return errf(http.StatusForbidden, "forbidden", "%s is %s: only someone at least %s converts keys to theirs (the keys would act with that role)", m.Email, m.Role, m.Role)
		}
		// The personal cap, as insertKey holds it against concurrent creates.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('lux_keys:' || $1 || ':' || $2))`, p.TenantID, m.ID); err != nil {
			return err
		}
		var have int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys k WHERE `+activeKey+` AND k.tenant_id = $1 AND k.user_id = $2`, p.TenantID, m.ID).Scan(&have); err != nil {
			return err
		}
		if have+sa.ActiveKeys > maxPersonalKeys {
			return errf(http.StatusConflict, "key_limit", "%s has %d active keys and %s %d: at most %d personal keys; revoke %d first", m.Email, have, sa.Name, sa.ActiveKeys, maxPersonalKeys, have+sa.ActiveKeys-maxPersonalKeys)
		}
		tag, err := tx.Exec(ctx, `UPDATE api_keys k SET user_id = $2, service_account_id = NULL WHERE k.service_account_id = $1 AND `+activeKey, sa.ID, m.ID)
		if err != nil {
			return err
		}
		out.Keys = int(tag.RowsAffected())
		tag, err = tx.Exec(ctx, `UPDATE runs SET owner_user_id = $3, owner_sa_id = NULL WHERE tenant_id = $1 AND owner_sa_id = $2`, p.TenantID, sa.ID, m.ID)
		if err != nil {
			return err
		}
		out.Runs = int(tag.RowsAffected())
		if err := detachAccountKeys(ctx, tx, p, sa); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM service_accounts WHERE id = $1`, sa.ID); err != nil {
			return err
		}
		if err := addTenantEvent(ctx, tx, p.TenantID, "service_account.converted", p, map[string]any{"serviceAccount": sa.ID, "name": sa.Name, "user": m.ID, "keys": out.Keys, "runs": out.Runs}); err != nil {
			return err
		}
		out.User, err = findMember(ctx, tx, p, m.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &convertOutput{out}, nil
}

// lockAccounts serializes deleting and converting a tenant's service
// accounts with membership changes (the members lock): a convert's target
// stays a member, with its role, until it commits, and two converts or
// deletes of one account run one after the other (the second finds none).
func lockAccounts(ctx context.Context, tx pgx.Tx, tenant string) error {
	return lockMembers(ctx, tx, tenant)
}

// detachAccountKeys revokes the account's active keys (key.revoked each,
// as removing a member does) and unties every key of it from the account,
// so the account's row can go; the key rows stay, revoked, for the record.
func detachAccountKeys(ctx context.Context, tx pgx.Tx, p Principal, sa ServiceAccount) error {
	rows, err := tx.Query(ctx, `UPDATE api_keys SET revoked_at = now() WHERE service_account_id = $1 AND revoked_at IS NULL RETURNING id, name`, sa.ID)
	if err != nil {
		return err
	}
	type key struct{ id, name string }
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.id, &k.name)
	})
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := addTenantEvent(ctx, tx, p.TenantID, "key.revoked", p, map[string]any{"key": k.id, "name": k.name, "serviceAccount": sa.ID}); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE api_keys SET service_account_id = NULL WHERE service_account_id = $1`, sa.ID)
	return err
}

func (s *Server) deleteServiceAccount(ctx context.Context, in *SAPath) (*struct{}, error) {
	p := principal(ctx)
	if !p.atLeast("admin") {
		return nil, errf(http.StatusForbidden, "forbidden", "only admins and owners delete service accounts")
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockAccounts(ctx, tx, p.TenantID); err != nil {
			return err
		}
		sa, _, err := loadAccount(ctx, tx, p.TenantID, in.SA)
		if err != nil {
			return err
		}
		if sa.Runs > 0 {
			he := errf(http.StatusConflict, "has_runs", "%s owns %d Runs: a service account with Runs cannot be deleted; disable it instead (its keys stop, its Runs stay attributed), or convert it to a member's personal keys", sa.Name, sa.Runs)
			he.Details = []string{fmt.Sprintf("runs: %d", sa.Runs)}
			return he
		}
		if err := detachAccountKeys(ctx, tx, p, sa); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM service_accounts WHERE id = $1`, sa.ID); err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, "service_account.deleted", p, map[string]any{"serviceAccount": sa.ID, "name": sa.Name, "role": sa.Role})
	})
	return nil, err
}

const saColumns = `sa.id, sa.name, sa.description, sa.role, sa.owner_user_id, sa.created_at, sa.created_by, sa.disabled_at,
	(SELECT max(k.last_used_at) FROM api_keys k WHERE k.service_account_id = sa.id),
	(SELECT count(*) FROM api_keys k WHERE k.service_account_id = sa.id AND ` + activeKey + `)::int,
	(SELECT count(*) FROM runs r WHERE r.tenant_id = sa.tenant_id AND r.owner_sa_id = sa.id)::int`

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
		err := r.Scan(&x.a.ID, &x.a.Name, &x.a.Description, &x.a.Role, &x.owner, &x.a.CreatedAt, &x.by, &x.a.DisabledAt, &x.a.LastUsedAt, &x.a.ActiveKeys, &x.a.Runs)
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

// accountOwner is memberByEmail for an account's owner: "" is no owner.
func accountOwner(ctx context.Context, tx pgx.Tx, tenant, email string) (*string, error) {
	if email == "" {
		return nil, nil
	}
	id, err := memberByEmail(ctx, tx, tenant, email)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// checkAccountRole: a service account is never an owner.
func checkAccountRole(role string) error {
	if role == "viewer" || role == "member" || role == "admin" {
		return nil
	}
	return errf(http.StatusUnprocessableEntity, "invalid_request", "role: viewer, member or admin")
}

// checkAccountName: a DNS label, as create takes it.
func checkAccountName(name string) error {
	if !dnsLabel.MatchString(name) {
		return errf(http.StatusUnprocessableEntity, "invalid_request", "name %q: lowercase letters, digits and dashes, starting and ending with a letter or digit, at most 63", name)
	}
	return nil
}

// renameAccount renames sa to name (409 conflict when another account of
// the tenant has it) and, with it, its active keys named exactly as the old
// name: the upgrade (093_people_backfill.sql) named each legacy key's
// account after the key, so such a key reads as the account's. Revoked
// keys keep their names, as the record. The keys renamed, by id.
func renameAccount(ctx context.Context, tx pgx.Tx, tenant string, sa ServiceAccount, name string) ([]string, error) {
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM service_accounts WHERE tenant_id = $1 AND name = $2 AND id <> $3)`, tenant, name, sa.ID).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, errf(http.StatusConflict, "conflict", "a service account %s exists in this tenant", name)
	}
	if _, err := tx.Exec(ctx, `UPDATE service_accounts SET name = $2 WHERE id = $1`, sa.ID, name); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return nil, errf(http.StatusConflict, "conflict", "a service account %s exists in this tenant", name)
		}
		return nil, err
	}
	rows, err := tx.Query(ctx, `UPDATE api_keys k SET name = $3 WHERE k.service_account_id = $1 AND k.name = $2 AND `+activeKey+` RETURNING k.id`, sa.ID, sa.Name, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
	if err := checkAccountName(b.Name); err != nil {
		return nil, err
	}
	if err := checkAccountRole(b.Role); err != nil {
		return nil, err
	}
	var out ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		owner, err := accountOwner(ctx, tx, p.TenantID, b.OwnerEmail)
		if err != nil {
			return err
		}
		id := ids.New(ids.ServiceAccount)
		tag, err := tx.Exec(ctx, `INSERT INTO service_accounts (id, tenant_id, name, description, role, owner_user_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (tenant_id, name) DO NOTHING`, id, p.TenantID, b.Name, b.Description, b.Role, owner, p.userRef())
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
	if b.Role != nil {
		if err := checkAccountRole(*b.Role); err != nil {
			return nil, err
		}
	}
	if b.Name != nil {
		if err := checkAccountName(*b.Name); err != nil {
			return nil, err
		}
	}
	var out ServiceAccount
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		sa, _, err := loadAccount(ctx, tx, p.TenantID, in.SA)
		if err != nil {
			return err
		}
		changed := map[string]any{"serviceAccount": sa.ID, "name": sa.Name}
		if b.Name != nil && *b.Name != sa.Name {
			keys, err := renameAccount(ctx, tx, p.TenantID, sa, *b.Name)
			if err != nil {
				return err
			}
			changed["name"], changed["from"], changed["to"] = *b.Name, sa.Name, *b.Name
			if len(keys) > 0 {
				changed["keysRenamed"] = keys
			}
		}
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
			owner, err := accountOwner(ctx, tx, p.TenantID, *b.OwnerEmail)
			if err != nil {
				return err
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
