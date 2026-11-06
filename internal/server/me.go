package server

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Person is someone who signs in: identified by the email Cloudflare
// Access verified.
type Person struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Picture  string `json:"picture"`
	Operator bool   `json:"operator"`
}

type Me struct {
	Person
	Memberships  []MembershipRef `json:"memberships"`
	LastSignInAt *time.Time      `json:"lastSignInAt,omitempty"`
}

type meOutput struct {
	Body Me
}

func (s *Server) peopleRoutes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "getMe", Method: http.MethodGet, Path: "/v1/me", Tags: []string{"people"},
		Summary: "Your profile",
		Description: "The person the request is from, signed in or by a personal key, with their tenants and their role in each. " +
			"A person in no tenant gets it too (any other route answers them 403 not_a_member). " +
			"404 for a service account's key or a platform key: no person.",
		Errors: []int{http.StatusNotFound},
	}, scopeSignin, s.getMe)
}

func errNoPerson() error {
	return errf(http.StatusNotFound, "not_found", "this key is no person's: a service account's, or a platform key")
}

func (s *Server) getMe(ctx context.Context, _ *struct{}) (*meOutput, error) {
	p := principal(ctx)
	if p.UserID == "" {
		return nil, errNoPerson()
	}
	var me Me
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id, email, name, picture, operator, last_seen_at FROM users WHERE id = $1`, p.UserID).
			Scan(&me.ID, &me.Email, &me.Name, &me.Picture, &me.Operator, &me.LastSignInAt); err != nil {
			return err
		}
		var err error
		me.Memberships, err = userMemberships(ctx, tx, p.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &meOutput{me}, nil
}

// peopleByIDs reads the people of ids (nil and empty skipped, duplicates
// read once) in one query, as the system: a key's creator, an inviter, a
// service account's owner, who may be of no tenant now. Lists hydrate from
// its map, not a query a row.
func peopleByIDs(ctx context.Context, tx pgx.Tx, ids ...*string) (map[string]*Person, error) {
	set := map[string]bool{}
	var list []string
	for _, id := range ids {
		if id != nil && *id != "" && !set[*id] {
			set[*id] = true
			list = append(list, *id)
		}
	}
	out := make(map[string]*Person, len(list))
	if len(list) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, email, name, picture, operator FROM users WHERE id = ANY($1)`, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Email, &p.Name, &p.Picture, &p.Operator); err != nil {
			return nil, err
		}
		out[p.ID] = &p
	}
	return out, rows.Err()
}

// person is m's person of id, or nil.
func person(m map[string]*Person, id *string) *Person {
	if id == nil {
		return nil
	}
	return m[*id]
}
