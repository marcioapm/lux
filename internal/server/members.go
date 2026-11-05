package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

type Member struct {
	Person
	Role        string     `json:"role"`
	JoinedAt    time.Time  `json:"joinedAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt,omitempty"`
	RunningRuns int        `json:"runningRuns"`
	KeyCount    *int       `json:"keyCount,omitempty"`
}
type MemberPath struct {
	User string `path:"user" doc:"The member's id or email."`
}
type memberOutput struct{ Body Member }
type membersOutput struct {
	Body struct {
		Members []Member `json:"members"`
	}
}
type roleInput struct {
	MemberPath
	Body struct {
		Role string `json:"role"`
	}
}

func (s *Server) memberRoutes(api huma.API) {
	register(s, api, huma.Operation{OperationID: "listMembers", Method: http.MethodGet, Path: "/v1/members", Tags: []string{"people"}, Summary: "List the tenant's members", Description: "Every role sees members; keyCount is shown only to self and admins."}, "read", forTenant(s.listMembers))
	register(s, api, huma.Operation{OperationID: "getMember", Method: http.MethodGet, Path: "/v1/members/{user}", Tags: []string{"people"}, Summary: "Get a member"}, "read", forTenant(s.getMember))
	register(s, api, huma.Operation{OperationID: "addMember", Method: http.MethodPost, Path: "/v1/members", Tags: []string{"people"}, Summary: "Add a member directly", Description: "Operators only. Normally invite the person instead."}, "operator", forTenant(s.addMember))
	register(s, api, huma.Operation{OperationID: "changeMemberRole", Method: http.MethodPatch, Path: "/v1/members/{user}", Tags: []string{"people"}, Summary: "Change a member's role", Description: "Admins manage roles up to admin; only owners and operators manage owners. Only an operator may demote the last owner (409 last_owner)."}, "admin", forTenant(s.changeMemberRole))
	register(s, api, huma.Operation{OperationID: "removeMember", Method: http.MethodDelete, Path: "/v1/members/{user}", Tags: []string{"people"}, Summary: "Remove a member", Description: "Revokes their keys in this tenant immediately; their Runs keep running and stay theirs. Their service accounts become ownerless. Only an operator may remove the last owner.", DefaultStatus: 204}, "admin", forTenant(s.removeMember))
	register(s, api, huma.Operation{OperationID: "listMemberKeys", Method: http.MethodGet, Path: "/v1/members/{user}/keys", Tags: []string{"people"}, Summary: "List a member's active keys", Description: "Self and admins only; everyone else gets 404."}, "read", forTenant(s.listMemberKeys))
}

func memberRows(ctx context.Context, tx pgx.Tx, p Principal, ref string) ([]Member, error) {
	rows, err := tx.Query(ctx, `SELECT u.id,u.email,u.name,u.picture,u.operator,m.role,m.created_at,u.last_seen_at,
 (SELECT count(*) FROM runs r WHERE r.tenant_id=m.tenant_id AND r.owner_user_id=u.id AND r.state IN ('running','starting','stopping'))::int,
 CASE WHEN $3 OR u.id=$4 THEN (SELECT count(*) FROM api_keys k WHERE k.tenant_id=m.tenant_id AND k.user_id=u.id AND `+activeKey+`)::int END
 FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND ($2='' OR u.id=$2 OR lower(u.email)=lower($2)) ORDER BY lower(u.email)`, p.TenantID, ref, p.atLeast("admin"), p.UserID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		err := r.Scan(&m.ID, &m.Email, &m.Name, &m.Picture, &m.Operator, &m.Role, &m.JoinedAt, &m.LastSeenAt, &m.RunningRuns, &m.KeyCount)
		return m, err
	})
	if out == nil {
		out = []Member{}
	}
	return out, err
}
func (s *Server) listMembers(ctx context.Context, _ *struct{}) (*membersOutput, error) {
	out := &membersOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		out.Body.Members, err = memberRows(ctx, tx, principal(ctx), "")
		return err
	})
	return out, err
}
func findMember(ctx context.Context, tx pgx.Tx, p Principal, ref string) (Member, error) {
	rows, err := memberRows(ctx, tx, p, ref)
	if err != nil {
		return Member{}, err
	}
	if len(rows) == 0 {
		return Member{}, errNotFound
	}
	return rows[0], nil
}
func (s *Server) getMember(ctx context.Context, in *MemberPath) (*memberOutput, error) {
	var m Member
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		m, err = findMember(ctx, tx, principal(ctx), in.User)
		return err
	})
	return &memberOutput{m}, err
}

// Serialize every membership mutation per tenant: two owners demoting or
// removing one another cannot both pass the last-owner count.
func lockMembers(ctx context.Context, tx pgx.Tx, tenant string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('lux_members:' || $1))`, tenant)
	return err
}
func checkOwnerChange(ctx context.Context, tx pgx.Tx, p Principal, old, new string) error {
	if !validRole(new) && new != "" {
		return errf(422, "invalid_request", "role: viewer, member, admin or owner")
	}
	if (old == "owner" || new == "owner") && !p.atLeast("owner") {
		return errf(403, "forbidden", "only an owner may manage owners")
	}
	if old == "owner" && new != "owner" && !p.Operator {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id=$1 AND role='owner'`, p.TenantID).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return errf(409, "last_owner", "the last owner cannot be removed or demoted: add another owner first")
		}
	}
	return nil
}
func (s *Server) changeMemberRole(ctx context.Context, in *roleInput) (*memberOutput, error) {
	p := principal(ctx)
	var out Member
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockMembers(ctx, tx, p.TenantID); err != nil {
			return err
		}
		m, err := findMember(ctx, tx, p, in.User)
		if err != nil {
			return err
		}
		if err := checkOwnerChange(ctx, tx, p, m.Role, in.Body.Role); err != nil {
			return err
		}
		if m.Role != in.Body.Role {
			if _, err := tx.Exec(ctx, `UPDATE memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2`, p.TenantID, m.ID, in.Body.Role); err != nil {
				return err
			}
			if err := addTenantEvent(ctx, tx, p.TenantID, "member.role_changed", p, map[string]any{"user": m.ID, "from": m.Role, "to": in.Body.Role}); err != nil {
				return err
			}
		}
		out, err = findMember(ctx, tx, p, m.ID)
		return err
	})
	return &memberOutput{out}, err
}
func (s *Server) removeMember(ctx context.Context, in *MemberPath) (*struct{}, error) {
	p := principal(ctx)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockMembers(ctx, tx, p.TenantID); err != nil {
			return err
		}
		m, err := findMember(ctx, tx, p, in.User)
		if err != nil {
			return err
		}
		if err := checkOwnerChange(ctx, tx, p, m.Role, ""); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE api_keys SET revoked_at=now() WHERE tenant_id=$1 AND user_id=$2 AND revoked_at IS NULL RETURNING id`, p.TenantID, m.ID)
		if err != nil {
			return err
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := addTenantEvent(ctx, tx, p.TenantID, "key.revoked", p, map[string]any{"key": k, "user": m.ID}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE service_accounts SET owner_user_id=NULL WHERE tenant_id=$1 AND owner_user_id=$2`, p.TenantID, m.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id=$1 AND user_id=$2`, p.TenantID, m.ID); err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, p.TenantID, "member.removed", p, map[string]any{"user": m.ID, "email": m.Email})
	})
	return nil, err
}
func (s *Server) addMember(ctx context.Context, in *struct {
	Body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
}) (*memberOutput, error) {
	p := principal(ctx)
	if !ValidAccessOperatorEmail(in.Body.Email) || !validRole(in.Body.Role) {
		return nil, errf(422, "invalid_request", "a valid email and role are required")
	}
	var out Member
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockMembers(ctx, tx, p.TenantID); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO users(id,email) VALUES($1,$2) ON CONFLICT(lower(email)) DO UPDATE SET email=users.email RETURNING id`, ids.New(ids.User), in.Body.Email).Scan(&id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO memberships(tenant_id,user_id,role,added_by) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.TenantID, id, in.Body.Role, p.Actor())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(409, "conflict", "already a member")
		}
		if err := addTenantEvent(ctx, tx, p.TenantID, "member.joined", p, map[string]any{"user": id, "role": in.Body.Role}); err != nil {
			return err
		}
		out, err = findMember(ctx, tx, p, id)
		return err
	})
	return &memberOutput{out}, err
}
func (s *Server) listMemberKeys(ctx context.Context, in *MemberPath) (*keysOutput, error) {
	p := principal(ctx)
	var keys []ApiKey
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		m, err := findMember(ctx, tx, p, in.User)
		if err != nil {
			return err
		}
		if m.ID != p.UserID && !p.atLeast("admin") {
			return errNotFound
		}
		keys, err = scanKeys(ctx, tx, `k.user_id=$1 AND k.tenant_id=$2`, m.ID, p.TenantID)
		return err
	})
	return &keysOutput{keyList{keys}}, err
}

var _ = errors.Is
