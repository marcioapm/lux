package server

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
	"net/http"
)

func (s *Server) validateSubmit(ctx context.Context, p Principal, stored spec.RunSpec, raw spec.Resources) (*submitRunOutput, error) {
	answer := &SubmitAnswer{Valid: true}
	var r pendingRun
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := checkRunQuota(ctx, tx, p.TenantID); err != nil {
			return err
		}
		rp, err := resolvePool(ctx, tx, p.TenantID, stored.Placement.Pool, stored.Placement.PoolID)
		if err != nil {
			return err
		}
		stored.Placement.Pool = rp.Name
		if _, _, err := resolveSubmitSize(ctx, tx, rp, raw, &stored.Resources); err != nil {
			return err
		}
		answer.EffectiveResources = &stored.Resources
		answer.Pool = rp.Name
		if rp.ID != nil {
			answer.PoolID = *rp.ID
		}
		r = pendingRun{TenantID: p.TenantID, Spec: stored, PoolID: rp.ID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Current host capacity is system-scoped; only the eligible host id and
	// aggregate blockers are returned, never other tenants' specs or usage.
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION READ ONLY`); err != nil {
			return err
		}
		ids, err := s.eligibleHostIDs(ctx, tx, []*string{r.PoolID}, []string{r.TenantID}, []string{""})
		if err != nil {
			return err
		}
		hs, err := s.candidateHosts(ctx, tx, ids)
		if err != nil {
			return err
		}
		var adm admission
		if r.Spec.Spot {
			demand, ok, err := normalDemand(ctx, tx, hs)
			if err != nil {
				return err
			}
			if !ok {
				adm.Blockers = []AdmissionBlocker{{Code: blockNormalPending}}
			} else {
				adm = spotAdmission(r, hs, demand, s.now())
			}
		} else {
			adm = normalAdmission(r, hs)
		}
		answer.Blockers = adm.Blockers
		if adm.Host != nil {
			answer.WouldStartNow = true
			answer.EligibleHost = adm.Host.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &submitRunOutput{Status: http.StatusOK, Body: answer}, nil
}
