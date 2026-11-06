package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Who may act on what, on top of scopes (docs/security.md). A member acts
// on the Runs they own, or that a service account they own owns (or, for
// an account's key, its own); an admin on every Run of the tenant.

// accessRule is what an operation needs beyond its scope.
type accessRule struct {
	// run: the Run its path names ({id}) must be one the caller may act on.
	run bool
	// server: the tenant server its path names ({id}): one attached to a
	// Run, as run; one attached to none, an admin.
	server bool
}

// accessRules: every operation that changes a Run, or a server of one.
// register attaches them; checkAccess enforces them.
var accessRules = map[string]accessRule{
	"patchRun": {run: true}, "timeoutRun": {run: true}, "stopRun": {run: true}, "resumeRun": {run: true},
	"terminateRun": {run: true}, "cancelRun": {run: true}, "pushRun": {run: true}, "syncRun": {run: true},
	"checkpointRun": {run: true}, "deleteArtifacts": {run: true}, "postInput": {run: true},
	"execRun": {run: true}, "attachRun": {run: true}, "portForward": {run: true},
	"addServer": {run: true}, "putServer": {run: true}, "startServer": {run: true}, "stopServer": {run: true},
	"restartServer": {run: true}, "removeServer": {run: true},
	"patchServer": {server: true}, "deleteServer": {server: true}, "attachServer": {server: true}, "detachServer": {server: true},
	"startTenantServer": {server: true}, "stopTenantServer": {server: true}, "restartTenantServer": {server: true},
}

const accessKey = "lux-access"

func (r accessRule) doc() string {
	if r.server {
		return "A member may change a server attached to a Run they may act on (see POST /v1/runs/{id}/stop); one attached to no Run, admins only: otherwise 403 not_run_owner (or forbidden)."
	}
	return "A member may do this to the Runs they own and those of a service account they own; an admin, to every Run of the tenant. Otherwise 403 not_run_owner, whose message names the owner."
}

// runOwner is who owns a Run: a person, a service account (and that
// account's owner, if any), or no one.
type runOwner struct {
	user, sa, saOwner *string
	// Names, for not_run_owner's message.
	name string
}

// canActOnRun: the caller may change the Run. An operator and an admin:
// any Run. A member: their own, their service accounts', or (a service
// account's key) the account's own. A viewer or a read-only key: none.
func (p Principal) canActOnRun(o runOwner) bool {
	if p.Operator || p.atLeast("admin") {
		return true
	}
	if !p.atLeast("member") {
		return false
	}
	switch {
	case p.UserID != "" && o.user != nil && *o.user == p.UserID:
		return true
	case p.ServiceAccountID != "" && o.sa != nil && *o.sa == p.ServiceAccountID:
		return true
	case p.UserID != "" && o.saOwner != nil && *o.saOwner == p.UserID:
		return true
	}
	return false
}

// loadRunOwner reads a Run's owner in the transaction's scope.
func loadRunOwner(ctx context.Context, tx pgx.Tx, runID string) (runOwner, error) {
	var o runOwner
	err := tx.QueryRow(ctx, `SELECT r.owner_user_id, r.owner_sa_id, sa.owner_user_id,
			coalesce(CASE WHEN r.owner_sa_id IS NOT NULL THEN 'service account ' || sa.name END, '')
		FROM runs r LEFT JOIN service_accounts sa ON sa.id = r.owner_sa_id WHERE r.id = $1`, runID).Scan(&o.user, &o.sa, &o.saOwner, &o.name)
	if err != nil || o.user == nil {
		return o, err
	}
	// users is the system's table; the tenant's view of it shows its
	// members and its Runs' owners only.
	err = tx.QueryRow(ctx, `SELECT CASE WHEN name <> '' THEN name || ' <' || email || '>' ELSE email END FROM users WHERE id = $1`, *o.user).Scan(&o.name)
	if errors.Is(err, pgx.ErrNoRows) {
		o.name, err = *o.user, nil
	}
	return o, err
}

func notRunOwner(o runOwner) error {
	if o.name == "" {
		return errf(http.StatusForbidden, "not_run_owner", "this Run has no owner: only an admin of the tenant may act on it")
	}
	return errf(http.StatusForbidden, "not_run_owner", "this Run is owned by %s: only they, or an admin of the tenant, may act on it", o.name)
}

func errUnattachedServer() error {
	return errf(http.StatusForbidden, "forbidden", "this server is attached to no Run: only an admin of the tenant may change it")
}

// checkRunActor is 403 not_run_owner unless the caller may act on the Run
// as tx reads it.
func checkRunActor(ctx context.Context, tx pgx.Tx, p Principal, runID string) error {
	o, err := loadRunOwner(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !p.canActOnRun(o) {
		return notRunOwner(o)
	}
	return nil
}

// requireRunActor is 403 not_run_owner unless the caller may act on the
// Run; a Run the scope does not see passes (the handler answers 404).
func (s *Server) requireRunActor(ctx context.Context, p Principal, runID string) error {
	if p.Operator || p.atLeast("admin") {
		return nil
	}
	return s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := checkRunActor(ctx, tx, p, runID); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
}

// requireServerActor: a server attached to a Run, as the Run; attached to
// none, an admin.
func (s *Server) requireServerActor(ctx context.Context, p Principal, serverID string) error {
	if p.Operator || p.atLeast("admin") {
		return nil
	}
	var run *string
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT run_id FROM run_servers WHERE id = $1`, serverID).Scan(&run)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if run == nil {
		return errUnattachedServer()
	}
	return s.requireRunActor(ctx, p, *run)
}

// lockedServerActor is requireServerActor for a server locked in tx (with
// its Run): the attachment checked is the one the change will act on.
func lockedServerActor(ctx context.Context, tx pgx.Tx, p Principal, v serverRow) error {
	if p.Operator || p.atLeast("admin") {
		return nil
	}
	if v.RunID == nil {
		return errUnattachedServer()
	}
	return checkRunActor(ctx, tx, p, *v.RunID)
}

// requireServerRunActor: a server created for a Run, as the Run; one for
// no Run (the tenant's own), an admin.
func (s *Server) requireServerRunActor(ctx context.Context, p Principal, runID string) error {
	if runID != "" {
		return s.requireRunActor(ctx, p, runID)
	}
	if !p.atLeast("admin") {
		return errf(http.StatusForbidden, "forbidden", "a server attached to no Run is the tenant's: only an admin may create one")
	}
	return nil
}

// checkAccess enforces the operation's accessRule, after authentication.
func (s *Server) checkAccess(ctx huma.Context, p Principal) error {
	rule, ok := ctx.Operation().Metadata[accessKey].(accessRule)
	if !ok {
		return nil
	}
	switch {
	case rule.run:
		return s.requireRunActor(ctx.Context(), p, ctx.Param("id"))
	case rule.server:
		return s.requireServerActor(ctx.Context(), p, ctx.Param("id"))
	}
	return nil
}

// objectTenant is the tenant owning the object the operation's path names,
// if it names one that exists: a person in several tenants acts in that
// one. Read as the system; the person's membership decides the rest.
func (s *Server) objectTenant(ctx huma.Context) (string, error) {
	o, ok := ctx.Operation().Metadata[ownerKey].(owner)
	if !ok {
		return "", nil
	}
	var t string
	err := s.db.Tx(ctx.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx.Context(), o.query, ctx.Param(o.param)).Scan(&t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return t, err
}
