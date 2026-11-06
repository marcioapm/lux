package server

import (
	"context"
	"testing"
)

func TestCostByMemberAndOwner(t *testing.T) {
	s, keys := costLabelFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO users(id,email,name) VALUES('usr_cost','cost@example.com','Cost Member')`)
	execSQL(t, s, ctx, `INSERT INTO memberships(tenant_id,user_id,role) VALUES('t1','usr_cost','member')`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_user_id='usr_cost' WHERE id='r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET owner_sa_id=(SELECT service_account_id FROM api_keys WHERE id='k1') WHERE id='r3'`)
	execSQL(t, s, ctx, `UPDATE service_accounts SET owner_user_id='usr_cost' WHERE id=(SELECT service_account_id FROM api_keys WHERE id='k1')`)
	for _, by := range []string{"member", "owner"} {
		var got CostSummaryBody
		if code := getJSON(t, s, keys["t1"], costPath("&by="+by+"&family=compute"), &got); code != 200 {
			t.Fatal(code)
		}
		sums := map[string]string{}
		for _, r := range got.Totals {
			sums[r.Key] = r.Amount
			if r.Key != "(none)" && r.Owner == nil {
				t.Error("missing owner", r)
			}
		}
		if by == "owner" {
			if sums["user:usr_cost"] != "12" || sums["(none)"] != "100" {
				t.Fatal(sums)
			}
		} else {
			if sums["user:usr_cost"] != "2" || sums["(none)"] != "100" || len(sums) != 3 {
				t.Fatal(sums)
			}
		}
	}
}
