package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// ConsoleAuth configures key or Cloudflare Access authentication.
type ConsoleAuth struct {
	Mode            string
	CFTeam, CFAud   string
	CFOperators     []string
	CFDefaultTenant string
}

// cfAccess verifies Cloudflare Access tokens: signed by the team's keys
// (fetched and cached by go-oidc), issued by the team, for this
// application, not expired. The user's name and picture come from Access's
// identity endpoint, cached by email.
type cfAccess struct {
	team     string // https://<team>.cloudflareaccess.com
	verifier *oidc.IDTokenVerifier
	client   *http.Client

	mu         sync.Mutex
	identities map[string]cachedIdentity
}

type cachedIdentity struct {
	identity
	until time.Time
}

// identity is what Access's identity endpoint says of a user.
type identity struct {
	Name string
	// Picture: the photo's URL from the identity provider's picture claim,
	// when Access passes it (the IdP's OIDC claims); https only.
	Picture string
}

func newCFAccess(team, aud string) *cfAccess {
	if !strings.Contains(team, ".") {
		team += ".cloudflareaccess.com"
	}
	if !strings.Contains(team, "://") {
		team = "https://" + team
	}
	team = strings.TrimRight(team, "/")
	keys := oidc.NewRemoteKeySet(context.Background(), team+"/cdn-cgi/access/certs")
	return &cfAccess{
		team:       team,
		verifier:   oidc.NewVerifier(team, keys, &oidc.Config{ClientID: aud}),
		client:     &http.Client{Timeout: 5 * time.Second},
		identities: map[string]cachedIdentity{},
	}
}

// accessToken is the request's Access token, if any: the header Access
// adds in front of luxd, or the browser's cookie. A cookie is sent with
// any request the browser makes, a cross-site one included, so it
// authenticates only reads, or requests that show they come from this
// origin (fetch sets Sec-Fetch-Site; a form post from elsewhere cannot
// fake it).
func accessToken(r *http.Request) string {
	if t := r.Header.Get("Cf-Access-Jwt-Assertion"); t != "" {
		return t
	}
	c, err := r.Cookie("CF_Authorization")
	if err != nil {
		return ""
	}
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !safe && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		return ""
	}
	return c.Value
}

// user verifies a token and says who it is. A service token (no email) is
// refused: the console is for people.
func (a *cfAccess) user(ctx context.Context, token string) (email string, id identity, err error) {
	t, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return "", identity{}, errf(http.StatusUnauthorized, "unauthorized", "invalid Cloudflare Access token: %v", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := t.Claims(&claims); err != nil || !ValidAccessOperatorEmail(claims.Email) {
		return "", identity{}, errf(http.StatusUnauthorized, "unauthorized", "the Cloudflare Access token names no valid user")
	}
	return claims.Email, a.lookupIdentity(ctx, claims.Email, token), nil
}

// ValidAccessOperatorEmail accepts a single plain ASCII mailbox.
func ValidAccessOperatorEmail(email string) bool {
	if email == "" || email != strings.TrimSpace(email) || !isASCII(email) {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email && strings.Contains(email, "@")
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// lookupIdentity is the user's name and picture from Access's identity
// endpoint (the token's claims carry only the email), cached for an hour;
// the name is the email if unknown. A failed lookup is cached for a minute
// only, so a blip does not hide the name for long.
func (a *cfAccess) lookupIdentity(ctx context.Context, email, token string) identity {
	a.mu.Lock()
	c, ok := a.identities[email]
	a.mu.Unlock()
	if ok && time.Now().Before(c.until) {
		return c.identity
	}
	id, ttl := identity{Name: email}, time.Minute
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.team+"/cdn-cgi/access/get-identity", nil)
	if err == nil {
		req.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: token})
		if resp, err := a.client.Do(req); err == nil {
			// Picture is any JSON: a provider that sends something other
			// than a string must not cost the name.
			var body struct {
				Name       string `json:"name"`
				OIDCFields struct {
					Picture any `json:"picture"`
				} `json:"oidc_fields"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil {
				if body.Name != "" {
					id.Name = body.Name
				}
				if pic, ok := body.OIDCFields.Picture.(string); ok {
					id.Picture = httpsURL(pic)
				}
				ttl = time.Hour
			}
			resp.Body.Close()
		}
	}
	a.mu.Lock()
	a.identities[email] = cachedIdentity{id, time.Now().Add(ttl)}
	a.mu.Unlock()
	return id
}

// httpsURL is s if it is an absolute https URL, else "".
func httpsURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return u.String()
}

// initCFTenant binds the configured name to its current ID before serving.
// An absent tenant leaves operator access available but no tenant access.
func (s *Server) initCFTenant(ctx context.Context) error {
	if s.cfAccess == nil || s.cfg.ConsoleAuth.CFDefaultTenant == "" {
		return nil
	}
	id, err := s.resolveTenant(ctx, s.cfg.ConsoleAuth.CFDefaultTenant)
	if he, ok := err.(*HTTPError); ok && he.Status == http.StatusNotFound {
		s.cfTenantID = ""
		s.log.Warn("Cloudflare Access default tenant is unavailable", "tenant", s.cfg.ConsoleAuth.CFDefaultTenant)
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve Cloudflare Access default tenant: %w", err)
	}
	s.cfTenantID = id
	return nil
}

func (s *Server) accessTenant(ctx context.Context) (string, error) {
	if s.cfTenantID == "" {
		return "", errf(http.StatusForbidden, "forbidden", "Cloudflare Access default tenant is unavailable")
	}
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1`, s.cfTenantID).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusForbidden, "forbidden", "Cloudflare Access default tenant is unavailable")
	}
	return id, err
}

// consoleUser authenticates an Access user: an allowlisted operator, or a
// member of the tenant the request is for (tenantRef: ?tenant= or the
// Lux-Tenant header; objRef: the tenant owning the object the path names,
// if any). Only the verified JWT email selects a role.
func (s *Server) consoleUser(r *http.Request, scope, tenantRef, objTenant string) (Principal, error) {
	if s.cfAccess == nil {
		return Principal{}, errf(http.StatusUnauthorized, "unauthorized", "missing API key")
	}
	token := accessToken(r)
	if token == "" {
		return Principal{}, errf(http.StatusUnauthorized, "unauthorized", "missing API key or Cloudflare Access token")
	}
	email, id, err := s.cfAccess.user(r.Context(), token)
	if err != nil {
		return Principal{}, err
	}
	return s.accessPrincipal(r.Context(), email, id, scope, tenantRef, objTenant)
}

// accessOperator: the email is on luxd's operator list.
func (s *Server) accessOperator(email string) bool {
	for _, allowed := range s.cfg.ConsoleAuth.CFOperators {
		if strings.EqualFold(email, allowed) {
			return true
		}
	}
	return false
}

var errNotAMember = errf(http.StatusForbidden, "not_a_member", "you are not a member of this tenant: ask one of its admins to invite this email")

// accessPrincipal is who a verified Access user is: an allowlisted
// operator (every tenant, as an operator key), or a person acting with
// their membership's role in one tenant: objTenant when the path names an
// object of a tenant they belong to, else the tenant tenantRef names, else
// their only membership (the default tenant's first, then the oldest). In
// no tenant: 403 not_a_member, except on routes of scope signin. Their
// users row is made on first sight and keeps their Access name and picture.
func (s *Server) accessPrincipal(ctx context.Context, email string, id identity, scope, tenantRef, objTenant string) (Principal, error) {
	p := Principal{Email: email, Name: id.Name, Picture: id.Picture}
	if len(s.cfg.ConsoleAuth.CFOperators) == 0 {
		return p, errf(http.StatusForbidden, "forbidden", "Cloudflare Access authorization is not configured")
	}
	p.Operator = s.accessOperator(email)
	ref := ""
	if !p.Operator && tenantRef != "" {
		t, err := s.resolveTenant(ctx, tenantRef)
		if err != nil {
			return p, errNotAMember
		}
		ref = t
	}
	var disabled bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var name, picture string
		var stale bool
		err := tx.QueryRow(ctx, `SELECT id, name, picture, disabled_at IS NOT NULL, coalesce(last_seen_at < now() - interval '1 minute', true)
			FROM users WHERE lower(email) = lower($1)`, email).Scan(&p.UserID, &name, &picture, &disabled, &stale)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO users (id, email, name, picture, operator, last_seen_at) VALUES ($1, $2, $3, $4, $5, now())
				ON CONFLICT (lower(email)) DO UPDATE SET last_seen_at = now() RETURNING id`,
				ids.New(ids.User), email, id.Name, id.Picture, p.Operator).Scan(&p.UserID)
		} else if err == nil && (stale || name != id.Name || picture != id.Picture) {
			_, err = tx.Exec(ctx, `UPDATE users SET name = $2, picture = $3, last_seen_at = now() WHERE id = $1`, p.UserID, id.Name, id.Picture)
		}
		if err != nil || p.Operator || disabled {
			return err
		}
		// The object's tenant first, if theirs; then the one asked for;
		// then a default.
		err = tx.QueryRow(ctx, `SELECT tenant_id, role FROM memberships WHERE user_id = $1
			AND ($2 = '' OR tenant_id = $2)
			ORDER BY tenant_id = $3 DESC, tenant_id = $4 DESC, created_at, tenant_id LIMIT 1`,
			p.UserID, ref, objTenant, s.cfTenantID).Scan(&p.TenantID, &p.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return p, err
	}
	if disabled {
		return p, errf(http.StatusForbidden, "forbidden", "this person is disabled")
	}
	if p.Operator {
		p.Scopes = []string{"operator"}
		return p, nil
	}
	if p.Role == "" {
		if scope == scopeSignin {
			return p, nil
		}
		return p, errNotAMember
	}
	p.Scopes = []string{roleScope(p.Role)}
	if !p.Can(scope) {
		return p, errf(http.StatusForbidden, "forbidden", "your role (%s) does not allow this: it needs the %q scope", p.Role, scope)
	}
	return p, nil
}

// previewAllowed is decision 13's gate, apart from memberships' 403: a
// verified Access user may open a preview of a server of tenant if they
// are an operator, a member of it (any role), or it is the default tenant
// (whose previews every Access user could always open).
func (s *Server) previewAllowed(ctx context.Context, email, tenant string) (bool, error) {
	if s.accessOperator(email) || (s.cfTenantID != "" && tenant == s.cfTenantID) {
		return true, nil
	}
	var ok bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
			WHERE m.tenant_id = $1 AND lower(u.email) = lower($2) AND u.disabled_at IS NULL)`, tenant, email).Scan(&ok)
	})
	return ok, err
}
