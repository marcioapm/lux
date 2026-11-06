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
		// Every membership in one query, grouped by person here.
		byUser := map[string][]MembershipRef{}
		rows, err = tx.Query(ctx, `SELECT m.user_id, m.tenant_id, t.name, m.role, m.created_at FROM memberships m JOIN tenants t ON t.id = m.tenant_id ORDER BY t.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var user string
			var m MembershipRef
			var at time.Time
			if err := rows.Scan(&user, &m.TenantID, &m.Tenant, &m.Role, &at); err != nil {
				return err
			}
			m.JoinedAt = &at
			byUser[user] = append(byUser[user], m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out.Body.People {
			p := &out.Body.People[i]
			if p.Memberships = byUser[p.ID]; p.Memberships == nil {
				p.Memberships = []MembershipRef{}
			}
		}
		return nil
	})
	return out, err
}
