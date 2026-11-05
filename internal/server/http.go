package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Principal is who a request is from: a person (by a personal key or
// signed in), a tenant's service account (by its key), or an operator. An
// operator belongs to no tenant; it sees and acts on every tenant.
type Principal struct {
	TenantID string
	KeyID    string
	Scopes   []string
	Operator bool
	// Email, Name and Picture (a photo's URL, if any): a person's.
	Email, Name, Picture string
	// UserID: the person's; ServiceAccountID: the account's, by its key.
	UserID, ServiceAccountID string
	// ServiceAccountName: with ServiceAccountID.
	ServiceAccountName string
	// Role in TenantID: the person's membership, or the account's role.
	// Empty for an operator, which outranks every role.
	Role string
	// ReadOnly: a key made read-only; it reads whatever the role is.
	ReadOnly bool
}

// atLeast: the principal holds role (or more) in its tenant, and may use
// it: a read-only key reads only.
func (p Principal) atLeast(role string) bool {
	if p.Operator {
		return true
	}
	if p.ReadOnly && roleRank(role) > roleRank("viewer") {
		return false
	}
	return roleRank(p.Role) >= roleRank(role)
}

// Actor names who a request is from in events: the person, or the key.
func (p Principal) Actor() string {
	if p.Email != "" {
		return p.Email
	}
	return p.KeyID
}

// scopeSignin is the pseudo-scope of the routes any authenticated caller
// may use, a person in no tenant included (their own profile, their
// invitations).
const scopeSignin = "signin"

func (p Principal) Can(scope string) bool {
	if scope == scopeSignin {
		return true
	}
	for _, s := range p.Scopes {
		// operator implies admin implies run implies read.
		if s == scope || s == "operator" || (s == "admin" && scope != "operator") || (s == "run" && scope == "read") {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	principalKey ctxKey = iota
	hostKey
	requestKey
)

// principal is who the request is from, put in its context by requireKey.
func principal(ctx context.Context) Principal { return ctx.Value(principalKey).(Principal) }

// HTTPError is an error with a status and a stable code. On the wire:
// {"error":{"code":..., "details":[...], "message":...}}.
type HTTPError struct {
	Status  int
	Code    string
	Message string
	Details []string
}

func (e *HTTPError) Error() string { return e.Message }

// GetStatus makes it a huma.StatusError.
func (e *HTTPError) GetStatus() int { return e.Status }

// errorBody is the error envelope; fields in the order the map-based
// encoding this replaced produced them.
type errorBody struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string   `json:"code" doc:"Stable error code, e.g. not_found, invalid_spec, quota_exceeded, pool_required, pool_preemptible, pool_has_non_preemptible_runs."`
	Details []string `json:"details,omitempty" doc:"Every problem, when there are several (invalid_spec, secrets_required, validation)."`
	Message string   `json:"message"`
}

func (e *HTTPError) MarshalJSON() ([]byte, error) {
	return json.Marshal(errorBody{errorInfo{Code: e.Code, Details: e.Details, Message: e.Message}})
}

func (e *HTTPError) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[errorBody](), true, "Error")
}

func errf(status int, code, f string, a ...any) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: fmt.Sprintf(f, a...)}
}

var errNotFound = errf(http.StatusNotFound, "not_found", "not found")

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if he := s.httpError(err, r.Method, r.URL.Path); he != nil {
		writeJSON(w, he.Status, he)
	}
}

// httpError maps a handler's error to what the client sees: nil when the
// client has gone. Anything unexpected is logged and hidden.
func (s *Server) httpError(err error, method, path string) *HTTPError {
	var he *HTTPError
	switch {
	case errors.As(err, &he):
		return he
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case errors.Is(err, context.Canceled):
		return nil
	}
	s.log.Error("request failed", "method", method, "path", path, "err", err)
	return errf(http.StatusInternalServerError, "internal", "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: %v", err)
	}
	return nil
}

// maxBody bounds a JSON request body.
const maxBody = 8 << 20

// authKey authenticates an API key and requires a scope. A person's key
// acts with the person's role in the key's tenant, a service account's
// with the account's; either only while the key has not expired, the
// person is still a member and not disabled, and the account not
// disabled. A read-only key reads only. A key with neither (an operator's
// key owned by no person, or a tenant key from before people) has its
// scopes.
func (s *Server) authKey(ctx context.Context, key, scope string) (Principal, error) {
	var p Principal
	if key == "" {
		return p, errf(http.StatusUnauthorized, "unauthorized", "missing API key")
	}
	var tenant, userID, saID, email, uname, picture, memberRole, saRole, saName *string
	var userOperator, userDisabled, saDisabled *bool
	var stale bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT k.tenant_id, k.id, k.scopes, coalesce(k.last_used_at < now() - interval '1 minute', true), k.read_only,
				k.user_id, k.service_account_id, u.email, u.name, u.picture, u.operator, u.disabled_at IS NOT NULL, m.role,
				sa.role, sa.name, sa.disabled_at IS NOT NULL
			FROM api_keys k
			LEFT JOIN users u ON u.id = k.user_id
			LEFT JOIN memberships m ON m.user_id = k.user_id AND m.tenant_id = k.tenant_id
			LEFT JOIN service_accounts sa ON sa.id = k.service_account_id
			WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())`, ids.Hash(key)).
			Scan(&tenant, &p.KeyID, &p.Scopes, &stale, &p.ReadOnly, &userID, &saID, &email, &uname, &picture, &userOperator, &userDisabled,
				&memberRole, &saRole, &saName, &saDisabled)
		if err != nil || !stale {
			return err
		}
		// last_used_at is coarse on purpose: writing it on every request
		// would put a row lock and a WAL write on every call.
		_, err = tx.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, p.KeyID)
		return err
	})
	invalid := errf(http.StatusUnauthorized, "unauthorized", "invalid API key")
	if errors.Is(err, pgx.ErrNoRows) {
		return p, invalid
	}
	if err != nil {
		return p, err
	}
	p.Operator = tenant == nil
	if tenant != nil {
		p.TenantID = *tenant
	}
	switch {
	case userID != nil:
		if *userDisabled {
			return p, invalid
		}
		p.UserID, p.Email, p.Name, p.Picture = *userID, *email, *uname, *picture
		if p.Operator {
			// An operator's own key: theirs while they are one.
			if !*userOperator {
				return p, invalid
			}
			break
		}
		if memberRole == nil {
			return p, invalid
		}
		p.Role = *memberRole
		p.Scopes = []string{roleScope(p.Role)}
	case saID != nil:
		if *saDisabled {
			return p, invalid
		}
		p.ServiceAccountID, p.ServiceAccountName, p.Role = *saID, *saName, *saRole
		// A key from before people keeps the scopes it lists while they
		// still say its account's role: whoami shows what it always did.
		if scopesRole(p.Scopes) != p.Role || p.ReadOnly {
			p.Scopes = []string{roleScope(p.Role)}
		}
	case !p.Operator:
		p.Role = scopesRole(p.Scopes)
	}
	if p.ReadOnly {
		p.Scopes = []string{"read"}
	}
	if !p.Can(scope) {
		if p.ReadOnly {
			return p, errf(http.StatusForbidden, "forbidden", "this key is read-only")
		}
		return p, errf(http.StatusForbidden, "forbidden", "this key lacks the %q scope", scope)
	}
	return p, nil
}

// narrow applies an operator's ?tenant= (an id or, failing that, a
// name): its request becomes one of that tenant. Tenant keys ignore it.
func (s *Server) narrow(ctx context.Context, p Principal, ref string) (Principal, error) {
	if !p.Operator || ref == "" {
		return p, nil
	}
	id, err := s.resolveTenant(ctx, ref)
	p.TenantID = id
	return p, err
}

func (s *Server) resolveTenant(ctx context.Context, ref string) (string, error) {
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1 OR name = $1 ORDER BY id = $1 DESC LIMIT 1`, ref).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusNotFound, "not_found", "no tenant %s", ref)
	}
	return id, err
}

// scope is the RLS scope of a read: the principal's tenant, or every
// tenant for an operator that did not narrow it.
func (p Principal) scope() store.Scope {
	if p.TenantID == "" {
		return store.System()
	}
	return store.Tenant(p.TenantID)
}

func bearer(r *http.Request) string { return bearerToken(r.Header.Get("Authorization")) }

func bearerToken(h string) string {
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/health" {
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
