package store_test

import (
	"os"
	"strings"
	"testing"
)

// A migration that holds live Runs' rows for long says so where deployers
// look, as 079 does: its procedure is in docs/operations.md (upgrading,
// step 2) with the count to take and the threshold, and the migration
// points there.
func TestLongMigrationsDocumented(t *testing.T) {
	ops, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	for mig, want := range map[string][]string{
		"079_run_first_pool_backfill": {"Above 300,000", "079 or 093 run with every luxd stopped"},
		"093_people_backfill": {"says otherwise (056; 062–064; 079; 093)", "093 then gives every Run an owner",
			"`SELECT count(*) FROM runs WHERE submitted_by_email IS NOT NULL OR submitted_by_key IS NOT NULL`",
			"Above 500,000", "079 or 093 run with every luxd stopped"},
	} {
		for _, w := range want {
			if !strings.Contains(string(ops), w) {
				t.Errorf("docs/operations.md: no %q for %s", w, mig)
			}
		}
		sql, err := os.ReadFile("migrations/" + mig + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(sql), "docs/operations.md (upgrading, step 2)") {
			t.Errorf("%s does not point at docs/operations.md (upgrading, step 2)", mig)
		}
	}
}
