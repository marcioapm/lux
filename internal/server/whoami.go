package server

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Whoami is who the caller is: an operator (every tenant), a person in a
// tenant, or a service account.
type Whoami struct {
	Operator bool   `json:"operator"`
	Tenant   string `json:"tenant" doc:"The caller's tenant's name; empty for an operator, or a person in no tenant."`
	TenantID string `json:"tenantId"`
	KeyID    string `json:"keyId,omitempty"`
	// Email and Name: a person's (signed in, or by a personal key).
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	// Picture: the person's photo URL, from the identity provider.
	Picture        string             `json:"picture,omitempty"`
	UserID         string             `json:"userId,omitempty" doc:"The person's id."`
	Role           string             `json:"role,omitempty" enum:"viewer,member,admin,owner" doc:"The caller's role in the tenant: the person's membership, or the service account's role. Absent for an operator."`
	ServiceAccount *ServiceAccountRef `json:"serviceAccount,omitempty" doc:"A service account's key: the account."`
	ReadOnly       bool               `json:"readOnly,omitempty" doc:"The key is read-only: it reads whatever the role is."`
	Memberships    []MembershipRef    `json:"memberships" doc:"The person's tenants and their role in each; empty for a service account or a platform key."`
	// InvitationsEnabled: invitations need verified emails, so exist only
	// with console auth cloudflare-access.
	InvitationsEnabled bool `json:"invitationsEnabled"`
	// ConsoleAuth is luxd's console auth: key or cloudflare-access.
	ConsoleAuth   string   `json:"consoleAuth"`
	Scopes        []string `json:"scopes"`
	Previews      bool     `json:"previews" doc:"Whether preview.domain is set, regardless of preview auth mode. Always present; its presence indicates support for single-label relative server hostnames. previewDomain remains null when previews are off or use Cloudflare Access."`
	ManagedImages bool     `json:"managedImages" doc:"Whether this luxd has a managed image registry ([images] repository and its credential): image.build.publish is accepted, and the Run image then has publishedRef."`
	ServerInject  bool     `json:"serverInject" doc:"Whether servers support inject: HTML the preview listener adds to their HTML pages. Always true on releases supporting server-inject."`
	// PreviewDomain: the console signs people in to hosts under
	// PreviewDomain only, with PreviewScheme and PreviewPort.
	PreviewDomain *string `json:"previewDomain" nullable:"true" doc:"The preview listener's domain (preview URLs are <scheme>://<server host>.<domain>); null when previews are off, or signed in to through Cloudflare Access rather than a ticket."`
	PreviewScheme string  `json:"previewScheme,omitempty" enum:"https,http" doc:"With previewDomain: the scheme of preview URLs (http only for a local demo domain under localhost)."`
	PreviewPort   int     `json:"previewPort,omitempty" doc:"With previewDomain: the port in preview URLs, if not the scheme's."`
}

type ServiceAccountRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MembershipRef struct {
	TenantID string     `json:"tenantId"`
	Tenant   string     `json:"tenant"`
	Role     string     `json:"role" enum:"viewer,member,admin,owner"`
	JoinedAt *time.Time `json:"joinedAt,omitempty"`
}

type whoamiOutput struct {
	Body Whoami
}

// whoami is GET /v1/whoami: the caller, so a client (the console) knows
// what to offer without probing.
func (s *Server) whoami(ctx context.Context, _ *struct{}) (*whoamiOutput, error) {
	p := principal(ctx)
	w := Whoami{Operator: p.Operator, KeyID: p.KeyID, Email: p.Email, Name: p.Name, Picture: p.Picture, Scopes: slices.Clone(p.Scopes),
		UserID: p.UserID, Role: p.Role, ReadOnly: p.ReadOnly, Memberships: []MembershipRef{}, InvitationsEnabled: s.invitationsEnabled(),
		ConsoleAuth: cmp.Or(s.cfg.ConsoleAuth.Mode, "key"), Previews: s.cfg.Preview.Domain != "", ServerInject: true, ManagedImages: s.cfg.Images.configured()}
	if p.Operator {
		w.Role = ""
	}
	if p.ServiceAccountID != "" {
		w.ServiceAccount = &ServiceAccountRef{ID: p.ServiceAccountID, Name: p.ServiceAccountName}
	}
	if s.previewTickets() {
		d := s.cfg.Preview.Domain
		w.PreviewDomain = &d
		w.PreviewScheme = cmp.Or(s.cfg.Preview.Scheme, "https")
		w.PreviewPort = s.cfg.Preview.PublicPort
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if !p.Operator && p.TenantID != "" {
			w.TenantID = p.TenantID
			if err := tx.QueryRow(ctx, `SELECT name FROM tenants WHERE id = $1`, p.TenantID).Scan(&w.Tenant); err != nil {
				return err
			}
		}
		if p.UserID == "" {
			return nil
		}
		var err error
		w.Memberships, err = userMemberships(ctx, tx, p.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &whoamiOutput{w}, nil
}

// userMemberships: a person's tenants, by tenant name. Needs the system scope.
func userMemberships(ctx context.Context, tx pgx.Tx, userID string) ([]MembershipRef, error) {
	rows, err := tx.Query(ctx, `SELECT m.tenant_id, t.name, m.role, m.created_at FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 ORDER BY t.name`, userID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MembershipRef, error) {
		var m MembershipRef
		var at time.Time
		err := r.Scan(&m.TenantID, &m.Tenant, &m.Role, &at)
		m.JoinedAt = &at
		return m, err
	})
	if out == nil {
		out = []MembershipRef{}
	}
	return out, err
}

// invitationsEnabled: invitations need a verified email, so they exist only
// with console auth cloudflare-access.
func (s *Server) invitationsEnabled() bool { return s.cfg.ConsoleAuth.Mode == "cloudflare-access" }
