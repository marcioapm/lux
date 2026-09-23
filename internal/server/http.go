package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Principal is who a request is from.
type Principal struct {
	TenantID string
	KeyID    string
	Scopes   []string
}

func (p Principal) Can(scope string) bool {
	for _, s := range p.Scopes {
		// admin implies run implies read.
		if s == scope || s == "admin" || (s == "run" && scope == "read") {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	principalKey ctxKey = iota
	hostKey
)

func principal(r *http.Request) Principal { return r.Context().Value(principalKey).(Principal) }

// HTTPError is an error with a status and a stable code.
type HTTPError struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *HTTPError) Error() string { return e.Message }

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
	var he *HTTPError
	if !errors.As(err, &he) {
		if errors.Is(err, pgx.ErrNoRows) {
			he = errNotFound
		} else if errors.Is(err, context.Canceled) {
			return
		} else {
			s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			he = errf(http.StatusInternalServerError, "internal", "internal error")
		}
	}
	body := map[string]any{"error": map[string]any{"code": he.Code, "message": he.Message}}
	if he.Details != nil {
		body["error"].(map[string]any)["details"] = he.Details
	}
	writeJSON(w, he.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: %v", err)
	}
	return nil
}

// withKey authenticates a tenant API key and requires a scope.
func (s *Server) withKey(scope string, h handler) http.HandlerFunc {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		key := bearer(r)
		if key == "" {
			return errf(http.StatusUnauthorized, "unauthorized", "missing API key")
		}
		var p Principal
		var stale bool
		err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
			err := tx.QueryRow(r.Context(), `
				SELECT tenant_id, id, scopes, coalesce(last_used_at < now() - interval '1 minute', true)
				FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, ids.Hash(key)).
				Scan(&p.TenantID, &p.KeyID, &p.Scopes, &stale)
			if err != nil || !stale {
				return err
			}
			// last_used_at is coarse on purpose: writing it on every request
			// would put a row lock and a WAL write on every call.
			_, err = tx.Exec(r.Context(), `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, p.KeyID)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(http.StatusUnauthorized, "unauthorized", "invalid API key")
		}
		if err != nil {
			return err
		}
		if !p.Can(scope) {
			return errf(http.StatusForbidden, "forbidden", "this key lacks the %q scope", scope)
		}
		return h(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
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
