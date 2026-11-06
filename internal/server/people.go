package server

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Roles, lowest first: each can do what the ones before it can.
var roles = []string{"viewer", "member", "admin", "owner"}

func roleRank(r string) int {
	for i, x := range roles {
		if x == r {
			return i + 1
		}
	}
	return 0
}

func validRole(r string) bool { return roleRank(r) > 0 }

// roleScope is the scope a role gets on the key ladder.
func roleScope(role string) string {
	switch role {
	case "viewer":
		return "read"
	case "member":
		return "run"
	case "admin", "owner":
		return "admin"
	}
	return ""
}

// scopesRole is the role a legacy key's scopes amount to.
func scopesRole(scopes []string) string {
	p := Principal{Scopes: scopes}
	switch {
	case p.Can("admin"):
		return "admin"
	case p.Can("run"):
		return "member"
	case p.Can("read"):
		return "viewer"
	}
	return ""
}

// upgradeDefaultTenant names the once-only step of the people upgrade.
const upgradeDefaultTenant = "people-default-tenant"

// syncPeople runs at startup. Every configured operator email has a users
// row with operator set, and no other row has it: an email taken off the
// list stops being an operator (its memberships stay). An operator key
// (luxd admin create-operator-key) named after an operator's email is that
// person's from then on. Once, the first time a default tenant is bound:
// its operators become its admins and every other email that submitted a
// Run there a member (the upgrade from before people; docs/operators.md).
func (s *Server) syncPeople(ctx context.Context) error {
	ops := []string{}
	for _, e := range s.cfg.ConsoleAuth.CFOperators {
		if e = strings.TrimSpace(e); e != "" {
			ops = append(ops, strings.ToLower(e))
		}
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for _, e := range ops {
			if _, err := tx.Exec(ctx, `INSERT INTO users (id, email, operator) VALUES ($1, $2, true)
				ON CONFLICT (lower(email)) DO UPDATE SET operator = true`, ids.New(ids.User), e); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET operator = false WHERE operator AND lower(email) <> ALL($1::text[])`, ops); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys k SET user_id = u.id FROM users u
			WHERE k.tenant_id IS NULL AND k.user_id IS NULL AND u.operator AND lower(k.name) = lower(u.email)`); err != nil {
			return err
		}
		if s.cfTenantID == "" {
			return nil
		}
		tag, err := tx.Exec(ctx, `INSERT INTO lux_upgrades (name) VALUES ($1) ON CONFLICT DO NOTHING`, upgradeDefaultTenant)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role, added_by)
			SELECT $1, id, 'admin', 'upgrade' FROM users WHERE operator ON CONFLICT DO NOTHING`, s.cfTenantID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role, added_by)
			SELECT DISTINCT $1, u.id, 'member', 'upgrade' FROM runs r JOIN users u ON lower(u.email) = lower(r.submitted_by_email)
			WHERE r.tenant_id = $1 ON CONFLICT DO NOTHING`, s.cfTenantID)
		return err
	})
}
