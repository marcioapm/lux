package server

import (
	"context"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
	"time"
)

type OperatorPerson struct {
	Person
	Memberships []MembershipRef `json:"memberships"`
	KeyCount    int             `json:"keyCount"`
	LastSeenAt  *time.Time      `json:"lastSeenAt,omitempty"`
}
type peopleOutput struct {
	Body struct {
		People []OperatorPerson `json:"people"`
	}
}

func (s *Server) operatorPeopleRoutes(api huma.API) {
	register(s, api, huma.Operation{OperationID: "listPeople", Method: "GET", Path: "/v1/people", Tags: []string{"operators"}, Summary: "List every person", Description: "Operators only: memberships, operator flag, key count and last seen."}, "operator", s.listPeople)
}
func (s *Server) listPeople(ctx context.Context, _ *struct{}) (*peopleOutput, error) {
	out := &peopleOutput{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT u.id,u.email,u.name,u.picture,u.operator,u.last_seen_at,(SELECT count(*) FROM api_keys k WHERE k.user_id=u.id AND `+activeKey+`)::int FROM users u ORDER BY lower(u.email)`)
		if err != nil {
			return err
		}
		out.Body.People, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OperatorPerson, error) {
			var p OperatorPerson
			err := r.Scan(&p.ID, &p.Email, &p.Name, &p.Picture, &p.Operator, &p.LastSeenAt, &p.KeyCount)
			return p, err
		})
		if err != nil {
			return err
		}
		if out.Body.People == nil {
			out.Body.People = []OperatorPerson{}
		}
		for i := range out.Body.People {
			p := &out.Body.People[i]
			p.Memberships, err = userMemberships(ctx, tx, p.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
