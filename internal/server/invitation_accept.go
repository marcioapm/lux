package server

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
	"net/http"
	"strings"
	"time"
)

func (s *Server) changeInvitation(ctx context.Context, id string, revoke bool) (*invitationOutput, error) {
	if err := s.requireInvitations(); err != nil {
		return nil, err
	}
	p := principal(ctx)
	var out Invitation
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockMembers(ctx, tx, p.TenantID); err != nil {
			return err
		}
		// By id: exactly that invitation (410 if it is spent). By email:
		// the live one (one per tenant and email); spent ones are history.
		list, err := s.readInvitations(ctx, tx, `i.tenant_id=$1 AND (i.id=$2 OR (lower(i.email)=lower($2) AND i.accepted_at IS NULL AND i.revoked_at IS NULL))`, p.TenantID, id)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errNotFound
		}
		v := list[0]
		if v.Role == "owner" && !p.atLeast("owner") {
			return errf(403, "forbidden", "only owners may manage owner invitations")
		}
		if v.AcceptedAt != nil || v.RevokedAt != nil {
			return errf(410, "invitation_unusable", "this invitation can't be used")
		}
		var by *string
		if p.UserID != "" {
			by = &p.UserID
		}
		typ := "invitation.renewed"
		if revoke {
			_, err = tx.Exec(ctx, `UPDATE invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1`, v.ID, by)
			typ = "invitation.revoked"
		} else {
			_, err = tx.Exec(ctx, `UPDATE invitations SET expires_at=now()+interval '14 days',renewed_at=now(),renewed_by=$2 WHERE id=$1`, v.ID, by)
		}
		if err != nil {
			return err
		}
		if err := addTenantEvent(ctx, tx, p.TenantID, typ, p, map[string]any{"invitation": v.ID, "email": v.Email}); err != nil {
			return err
		}
		list, err = s.readInvitations(ctx, tx, `i.id=$1`, v.ID)
		if err == nil {
			out = list[0]
		}
		return err
	})
	return &invitationOutput{out}, err
}
func (s *Server) renewInvitation(ctx context.Context, in *InvitationPath) (*invitationOutput, error) {
	return s.changeInvitation(ctx, in.ID, false)
}
func (s *Server) revokeInvitation(ctx context.Context, in *InvitationPath) (*invitationOutput, error) {
	return s.changeInvitation(ctx, in.ID, true)
}

func (s *Server) lookupInvitation(ctx context.Context, in *InvitationCode) (*inviteLookupOutput, error) {
	if err := s.verifiedPerson(ctx); err != nil {
		return nil, err
	}
	p := principal(ctx)
	out := &inviteLookupOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		list, err := s.readInvitations(ctx, tx, `i.code=$1`, in.Code)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errf(410, "invitation_unusable", "this invitation can't be used")
		}
		v := list[0]
		out.Body.Status = v.Status
		if !strings.EqualFold(v.Email, p.Email) {
			return nil
		}
		out.Body.EmailMatches = true
		out.Body.Role = v.Role
		out.Body.ExpiresAt = &v.ExpiresAt
		if v.InvitedBy != nil {
			out.Body.InvitedBy = &InviterName{Name: v.InvitedBy.Name}
			if out.Body.InvitedBy.Name == "" {
				out.Body.InvitedBy.Name = v.InvitedBy.Email
			}
		}
		return tx.QueryRow(ctx, `SELECT t.name FROM invitations i JOIN tenants t ON t.id=i.tenant_id WHERE i.code=$1`, in.Code).Scan(&out.Body.Tenant)
	})
	return out, err
}
func (s *Server) acceptInvitation(ctx context.Context, in *InvitationCode) (*inviteAcceptOutput, error) {
	if err := s.verifiedPerson(ctx); err != nil {
		return nil, err
	}
	p := principal(ctx)
	out := &inviteAcceptOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Same lock as member mutations, then the invitation's row lock. Two
		// accepts cannot create two memberships or both consume the code.
		var tenant string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM invitations WHERE code=$1`, in.Code).Scan(&tenant); err != nil {
			if err == pgx.ErrNoRows {
				return errf(410, "invitation_unusable", "this invitation can't be used")
			}
			return err
		}
		if err := lockMembers(ctx, tx, tenant); err != nil {
			return err
		}
		var id, email, role string
		var expires time.Time
		var used bool
		if err := tx.QueryRow(ctx, `SELECT id,email,role,expires_at,accepted_at IS NOT NULL OR revoked_at IS NOT NULL FROM invitations WHERE code=$1 FOR UPDATE`, in.Code).Scan(&id, &email, &role, &expires, &used); err != nil {
			return err
		}
		// Check email before reporting anything about usability.
		if !strings.EqualFold(email, p.Email) {
			return errf(403, "invitation_email_mismatch", "this invitation is for someone else")
		}
		if used || !expires.After(time.Now()) {
			return errf(410, "invitation_unusable", "this invitation can't be used")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships(tenant_id,user_id,role,added_by,invitation_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, tenant, p.UserID, role, p.Actor(), id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invitations SET accepted_at=now(),accepted_user_id=$2 WHERE id=$1`, id, p.UserID); err != nil {
			return err
		}
		out.Body.TenantID = tenant
		if err := tx.QueryRow(ctx, `SELECT t.name,m.role FROM tenants t JOIN memberships m ON m.tenant_id=t.id WHERE t.id=$1 AND m.user_id=$2`, tenant, p.UserID).Scan(&out.Body.Tenant, &out.Body.Role); err != nil {
			return err
		}
		return addTenantEvent(ctx, tx, tenant, "member.joined", p, map[string]any{"user": p.UserID, "invitation": id, "role": out.Body.Role})
	})
	return out, err
}

type pendingInvitation struct {
	Code      string       `json:"code"`
	Tenant    string       `json:"tenant"`
	Role      string       `json:"role"`
	InvitedBy *InviterName `json:"invitedBy,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
	ExpiresAt time.Time    `json:"expiresAt"`
}
type pendingInvitationsOutput struct {
	Body struct {
		Invitations []pendingInvitation `json:"invitations"`
	}
}

func (s *Server) myInvitations(ctx context.Context, _ *struct{}) (*pendingInvitationsOutput, error) {
	if err := s.verifiedPerson(ctx); err != nil {
		return nil, err
	}
	out := &pendingInvitationsOutput{}
	out.Body.Invitations = []pendingInvitation{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT i.code,t.name,i.role,coalesce(nullif(u.name,''),u.email),i.created_at,i.expires_at FROM invitations i JOIN tenants t ON t.id=i.tenant_id LEFT JOIN users u ON u.id=i.invited_by WHERE lower(i.email)=lower($1) AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at>now() ORDER BY i.created_at`, principal(ctx).Email)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v pendingInvitation
			var name *string
			if err := rows.Scan(&v.Code, &v.Tenant, &v.Role, &name, &v.CreatedAt, &v.ExpiresAt); err != nil {
				return err
			}
			if name != nil {
				v.InvitedBy = &InviterName{Name: *name}
			}
			out.Body.Invitations = append(out.Body.Invitations, v)
		}
		return rows.Err()
	})
	return out, err
}

type createTenantInput struct {
	Body struct {
		Name       string `json:"name"`
		OwnerEmail string `json:"ownerEmail,omitempty"`
	}
}
type createTenantOutput struct {
	Body struct {
		ID         string      `json:"id"`
		Name       string      `json:"name"`
		TenantID   string      `json:"tenantId"`
		Invitation *Invitation `json:"invitation,omitempty"`
	}
}

func (s *Server) createTenant(ctx context.Context, in *createTenantInput) (*createTenantOutput, error) {
	b := in.Body
	if strings.TrimSpace(b.Name) == "" {
		return nil, errf(422, "invalid_request", "name is required")
	}
	if b.OwnerEmail != "" {
		if err := s.requireInvitations(); err != nil {
			return nil, err
		}
		if !ValidAccessOperatorEmail(b.OwnerEmail) {
			return nil, errf(422, "invalid_request", "invalid owner email")
		}
	}
	out := &createTenantOutput{}
	out.Body.ID = ids.New(ids.Tenant)
	out.Body.TenantID = out.Body.ID
	out.Body.Name = b.Name
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants(id,name) VALUES($1,$2) ON CONFLICT(name) DO NOTHING`, out.Body.ID, b.Name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(409, "conflict", "tenant name already exists")
		}
		if b.OwnerEmail != "" {
			p := principal(ctx)
			p.TenantID = out.Body.ID
			r, err := s.inviteOne(ctx, tx, p, b.OwnerEmail, "", "owner")
			if err != nil {
				return err
			}
			out.Body.Invitation = r.Invitation
		}
		return nil
	})
	return out, err
}

var _ = http.MethodGet
