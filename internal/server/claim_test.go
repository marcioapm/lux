package server

import (
	"context"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

// claimRuns gives the Runs of key's tenant that have no owner to key's
// service account: fixtures that insert Runs by SQL and act on them with a
// tenant key, as their submitter would.
func claimRuns(t *testing.T, s *Server, key string) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE runs r SET owner_sa_id = k.service_account_id FROM api_keys k
		WHERE k.key_hash = $1 AND k.service_account_id IS NOT NULL AND r.tenant_id = k.tenant_id AND r.owner_user_id IS NULL AND r.owner_sa_id IS NULL`, ids.Hash(key))
}
