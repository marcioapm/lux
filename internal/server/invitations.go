package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

type Invitation struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name,omitempty"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	Code       string     `json:"code"`
	URL        string     `json:"url"`
	InvitedBy  *Person    `json:"invitedBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RenewedAt  *time.Time `json:"renewedAt,omitempty"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	RevokedBy  *Person    `json:"revokedBy,omitempty"`
}
type InvitationPath struct {
	ID string `path:"invitation"`
}
type InvitationCode struct {
	Code string `path:"code"`
}
type invitationOutput struct{ Body Invitation }
type inviteResult struct {
	Email      string      `json:"email"`
	Result     string      `json:"result"`
	Invitation *Invitation `json:"invitation,omitempty"`
	Message    string      `json:"message,omitempty"`
}
type inviteBatch struct {
	Invitations []struct {
		Email string `json:"email"`
		Name  string `json:"name,omitempty"`
	} `json:"invitations"`
	Role string `json:"role"`
}
type inviteBatchOutput struct {
	Body struct {
		Results []inviteResult `json:"results"`
	}
}
type inviteListInput struct {
	Status string `query:"status"`
	Q      string `query:"q"`
	Cursor string `query:"cursor"`
}
type inviteListOutput struct {
	Body struct {
		Invitations []Invitation `json:"invitations"`
		Cursor      string       `json:"cursor"`
	}
}
type InvitationCounts struct {
	Pending  int `json:"pending"`
	Expired  int `json:"expired"`
	Accepted int `json:"accepted"`
	Revoked  int `json:"revoked"`
}
type inviteCountsOutput struct{ Body InvitationCounts }
type InviterName struct {
	Name string `json:"name"`
}
type inviteLookupOutput struct {
	Body struct {
		Tenant       string       `json:"tenant,omitempty"`
		Role         string       `json:"role,omitempty"`
		InvitedBy    *InviterName `json:"invitedBy,omitempty"`
		ExpiresAt    *time.Time   `json:"expiresAt,omitempty"`
		Status       string       `json:"status"`
		EmailMatches bool         `json:"emailMatches"`
	}
}
type inviteAcceptOutput struct {
	Body struct {
		TenantID string `json:"tenantId"`
		Tenant   string `json:"tenant"`
		Role     string `json:"role"`
	}
}

func (s *Server) invitationRoutes(api huma.API) {
	register(s, api, huma.Operation{OperationID: "createInvitations", Method: "POST", Path: "/v1/invitations", Tags: []string{"people"}, Summary: "Invite people to the tenant", Description: "One email per invitation, valid 14 days. Returns each email's created, existing, renewed, member or invalid result. No email is sent: send the links yourself. Only owners invite owners. Key console mode: 409 invitations_unavailable."}, "admin", forTenant(s.createInvitations))
	register(s, api, huma.Operation{OperationID: "listInvitations", Method: "GET", Path: "/v1/invitations", Tags: []string{"people"}, Summary: "List invitations", Description: "status: pending, expired, accepted, revoked or all (default). q searches email. Up to 100, by id; cursor continues."}, "admin", forTenant(s.listInvitations))
	register(s, api, huma.Operation{OperationID: "invitationCounts", Method: "GET", Path: "/v1/invitations/counts", Tags: []string{"people"}, Summary: "Count invitations by status"}, "admin", forTenant(s.invitationCounts))
	register(s, api, huma.Operation{OperationID: "renewInvitation", Method: "POST", Path: "/v1/invitations/{invitation}/renew", Tags: []string{"people"}, Summary: "Renew an invitation", Description: "Pending or expired only: the same code, valid 14 more days."}, "admin", forTenant(s.renewInvitation))
	register(s, api, huma.Operation{OperationID: "revokeInvitation", Method: "POST", Path: "/v1/invitations/{invitation}/revoke", Tags: []string{"people"}, Summary: "Revoke an invitation"}, "admin", forTenant(s.revokeInvitation))
	register(s, api, huma.Operation{OperationID: "lookupInvitation", Method: "GET", Path: "/v1/invitations/by-code/{code}", Tags: []string{"people"}, Summary: "Look up an invitation", Description: "Verified Access users only. The intended email sees tenant, role, inviter name, expiry and status; anyone else only status and emailMatches:false."}, scopeSignin, s.lookupInvitation)
	register(s, api, huma.Operation{OperationID: "acceptInvitation", Method: "POST", Path: "/v1/invitations/by-code/{code}/accept", Tags: []string{"people"}, Summary: "Accept an invitation", Description: "Verified Access users only; email must match (403 invitation_email_mismatch, with no invitation details). Expired, revoked or accepted: 410 invitation_unusable."}, scopeSignin, s.acceptInvitation)
	register(s, api, huma.Operation{OperationID: "myInvitations", Method: "GET", Path: "/v1/me/invitations", Tags: []string{"people"}, Summary: "Your pending invitations", Description: "Verified Access users only, including people not yet a member of any tenant."}, scopeSignin, s.myInvitations)
	register(s, api, huma.Operation{OperationID: "createTenant", Method: "POST", Path: "/v1/tenants", Tags: []string{"operators"}, Summary: "Create a tenant", Description: "Optionally invite its first owner (verified Access mode only)."}, "operator", s.createTenant)
}
func (s *Server) requireInvitations() error {
	if !s.invitationsEnabled() {
		return errf(409, "invitations_unavailable", "invitations require cloudflare-access console auth")
	}
	return nil
}
func (s *Server) verifiedPerson(ctx context.Context) error {
	if err := s.requireInvitations(); err != nil {
		return err
	}
	p := principal(ctx)
	if p.KeyID != "" || p.UserID == "" {
		return errf(401, "unauthorized", "a verified Cloudflare Access email is required")
	}
	return nil
}

const inviteStatusSQL = `CASE WHEN i.revoked_at IS NOT NULL THEN 'revoked' WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END`
const inviteColumns = `i.id,i.email,i.name,i.role,i.code,i.created_at,i.expires_at,i.renewed_at,i.accepted_at,i.revoked_at,i.invited_by,i.revoked_by,` + inviteStatusSQL

func (s *Server) inviteURL(ctx context.Context, code string) string {
	origin := strings.TrimRight(s.cfg.PublicURL, "/")
	if origin == "" {
		if r, ok := ctx.Value(requestKey).(*http.Request); ok {
			scheme := "https"
			if r.TLS == nil {
				scheme = "http"
			}
			origin = scheme + "://" + r.Host
		}
	}
	if u, err := url.Parse(origin); err == nil {
		u.Path = ""
		u.RawQuery = ""
		u.Fragment = ""
		origin = strings.TrimRight(u.String(), "/")
	}
	return origin + "/invite/" + code
}
func (s *Server) readInvitations(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]Invitation, error) {
	rows, err := tx.Query(ctx, `SELECT `+inviteColumns+` FROM invitations i WHERE `+where+` ORDER BY i.id LIMIT 101`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		v       Invitation
		by, rev *string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		v := &x.v
		err := r.Scan(&v.ID, &v.Email, &v.Name, &v.Role, &v.Code, &v.CreatedAt, &v.ExpiresAt, &v.RenewedAt, &v.AcceptedAt, &v.RevokedAt, &x.by, &x.rev, &v.Status)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, 0, len(list))
	for _, x := range list {
		x.v.URL = s.inviteURL(ctx, x.v.Code)
		if x.v.InvitedBy, err = personByID(ctx, tx, x.by); err != nil {
			return nil, err
		}
		if x.v.RevokedBy, err = personByID(ctx, tx, x.rev); err != nil {
			return nil, err
		}
		out = append(out, x.v)
	}
	return out, nil
}
func (s *Server) inviteOne(ctx context.Context, tx pgx.Tx, p Principal, email, name, role string) (inviteResult, error) {
	res := inviteResult{Email: email}
	if !ValidAccessOperatorEmail(email) {
		res.Result = "invalid"
		res.Message = "not a valid email"
		return res, nil
	}
	if err := lockMembers(ctx, tx, p.TenantID); err != nil {
		return res, err
	}
	var member bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND lower(u.email)=lower($2))`, p.TenantID, email).Scan(&member); err != nil {
		return res, err
	}
	if member {
		res.Result = "member"
		res.Message = "already a member of this tenant"
		return res, nil
	}
	list, err := s.readInvitations(ctx, tx, `i.tenant_id=$1 AND lower(i.email)=lower($2) AND i.accepted_at IS NULL AND i.revoked_at IS NULL`, p.TenantID, email)
	if err != nil {
		return res, err
	}
	var id string
	var by *string
	if p.UserID != "" {
		by = &p.UserID
	}
	if len(list) > 0 {
		v := list[0]
		id = v.ID
		if v.Role == "owner" && !p.atLeast("owner") {
			return res, errf(403, "forbidden", "only owners may manage owner invitations")
		}
		if v.Status == "pending" {
			res.Result = "existing"
			res.Invitation = &v
			return res, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE invitations SET expires_at=now()+interval '14 days',renewed_at=now(),renewed_by=$2 WHERE id=$1`, id, by); err != nil {
			return res, err
		}
		res.Result = "renewed"
		if err := addTenantEvent(ctx, tx, p.TenantID, "invitation.renewed", p, map[string]any{"invitation": id, "email": email}); err != nil {
			return res, err
		}
	} else {
		id = ids.New(ids.Invitation)
		_, err := tx.Exec(ctx, `INSERT INTO invitations(id,tenant_id,code,email,name,role,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '14 days')`, id, p.TenantID, ids.Secret("invite"), email, name, role, by)
		if err != nil {
			return res, err
		}
		res.Result = "created"
		if err := addTenantEvent(ctx, tx, p.TenantID, "member.invited", p, map[string]any{"invitation": id, "email": email, "role": role}); err != nil {
			return res, err
		}
	}
	list, err = s.readInvitations(ctx, tx, `i.id=$1`, id)
	if err != nil {
		return res, err
	}
	res.Invitation = &list[0]
	return res, nil
}
func (s *Server) createInvitations(ctx context.Context, in *struct{ Body inviteBatch }) (*inviteBatchOutput, error) {
	if err := s.requireInvitations(); err != nil {
		return nil, err
	}
	p := principal(ctx)
	if !validRole(in.Body.Role) {
		return nil, errf(422, "invalid_request", "invalid role")
	}
	if in.Body.Role == "owner" && !p.atLeast("owner") {
		return nil, errf(403, "forbidden", "only owners may invite owners")
	}
	if len(in.Body.Invitations) > 100 {
		return nil, errf(422, "invalid_request", "at most 100 invitations per batch")
	}
	out := &inviteBatchOutput{}
	out.Body.Results = []inviteResult{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for _, i := range in.Body.Invitations {
			r, err := s.inviteOne(ctx, tx, p, strings.TrimSpace(i.Email), i.Name, in.Body.Role)
			if err != nil {
				return err
			}
			out.Body.Results = append(out.Body.Results, r)
		}
		return nil
	})
	return out, err
}
func (s *Server) listInvitations(ctx context.Context, in *inviteListInput) (*inviteListOutput, error) {
	if err := s.requireInvitations(); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = "all"
	}
	switch status {
	case "all", "pending", "expired", "accepted", "revoked":
	default:
		return nil, errf(422, "invalid_request", "invalid status")
	}
	out := &inviteListOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		list, err := s.readInvitations(ctx, tx, `i.tenant_id=$1 AND ($2='all' OR `+inviteStatusSQL+`=$2) AND position(lower($3) in lower(i.email))>0 AND i.id>$4`, principal(ctx).TenantID, status, in.Q, in.Cursor)
		if err != nil {
			return err
		}
		if len(list) > 100 {
			out.Body.Cursor = list[99].ID
			list = list[:100]
		}
		out.Body.Invitations = list
		return nil
	})
	return out, err
}
func (s *Server) invitationCounts(ctx context.Context, _ *struct{}) (*inviteCountsOutput, error) {
	if err := s.requireInvitations(); err != nil {
		return nil, err
	}
	out := &inviteCountsOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='pending'),count(*) FILTER(WHERE status='expired'),count(*) FILTER(WHERE status='accepted'),count(*) FILTER(WHERE status='revoked') FROM(SELECT `+inviteStatusSQL+` status FROM invitations i WHERE i.tenant_id=$1)s`, principal(ctx).TenantID).Scan(&out.Body.Pending, &out.Body.Expired, &out.Body.Accepted, &out.Body.Revoked)
	})
	return out, err
}
