package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

// topFixture: t1 Runs a1..a5 labelled app=a1..a5 and n1 without app, each
// costing USD compute per hour for two hours (a1 most), a5 also EUR ai, and
// a4 USD ai only.
func topFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	for i, amount := range []string{"50", "40", "30", "20", "10"} {
		id := fmt.Sprintf("a%d", i+1)
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ($1, 't1', '{}', 'running', jsonb_build_object('app', $1::text))`, id)
		execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
			VALUES ($1, 't1', $3, 'compute', 'compute', 'USD', $4::numeric, 0, 0), ($2, 't1', $3, 'compute', 'compute', 'USD', $4::numeric, 0, 0)`,
			t0, t0.Add(time.Hour), id, amount)
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ('n1', 't1', '{}', 'running', '{}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated) VALUES
		($1, 't1', 'n1', 'compute', 'compute', 'USD', 1000, 0, 0),
		($1, 't1', 'a5', 'plugin', 'ai', 'EUR', 9, 0, 0),
		($1, 't1', 'a1', 'plugin', 'ai', 'EUR', 1, 0, 0),
		($1, 't1', 'a3', 'plugin', 'ai', 'EUR', 1, 0, 0),
		($1, 't1', 'a4', 'plugin', 'ai', 'USD', 100, 0, 0)`, t0)
	return s, keys
}

// byGroup is totals as "value currency" -> amount for group key g.
func byGroup(rows []CostSummaryRow, g string) map[string]string {
	m := map[string]string{}
	for _, r := range rows {
		m[r.Group[g]+" "+r.Currency] = r.Amount
	}
	return m
}

func TestCostSummaryTop(t *testing.T) {
	s, keys := topFixture(t)
	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&top=2&interval=hour"), &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	// USD all: a4 140, a1 100, then a2 80, a3 60, a5 20 folded; (none) 1000 kept and not counted.
	// EUR: a5 9, then a1 1 and a3 1 tie broken by value: a1 kept, a3 folded.
	want := map[string]string{
		"a4 USD": "140", "a1 USD": "100", "(other) USD": "160", "(none) USD": "1000",
		"a5 EUR": "9", "a1 EUR": "1", "(other) EUR": "1",
	}
	if m := byGroup(got.Totals, "label:app"); !maps.Equal(m, want) {
		t.Errorf("totals %v, want %v", m, want)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 3, "EUR": 1}) {
		t.Errorf("otherCount %v", got.OtherCount)
	}
	runs := map[string]int{}
	for _, r := range got.Totals {
		if r.Runs == nil {
			t.Fatalf("totals row without runs: %+v", r)
		}
		runs[r.Group["label:app"]+" "+r.Currency] = *r.Runs
	}
	if !maps.Equal(runs, map[string]int{"a4 USD": 1, "a1 USD": 1, "(other) USD": 3, "(none) USD": 1, "a5 EUR": 1, "a1 EUR": 1, "(other) EUR": 1}) {
		t.Errorf("runs %v", runs)
	}
	// The series folds the same way: every bucket sums to the totals.
	series := map[string]float64{}
	for _, r := range got.Series {
		if r.Runs != nil {
			t.Errorf("series row with runs: %+v", r)
		}
		var f float64
		fmt.Sscan(r.Amount, &f)
		series[r.Group["label:app"]+" "+r.Currency] += f
	}
	if series["(other) USD"] != 160 || series["(none) USD"] != 1000 || series["a4 USD"] != 140 || len(series) != len(want) {
		t.Errorf("series sums %v", series)
	}

	// Rank by compute: a4's AI no longer lifts it; the second group is kept under the fold.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&group=family&top=2&rank=compute"), &got); code != http.StatusOK {
		t.Fatalf("rank status %d", code)
	}
	split := map[string]string{}
	for _, r := range got.Totals {
		split[r.Group["label:app"]+" "+r.Group["family"]+" "+r.Currency] = r.Amount
	}
	wantSplit := map[string]string{
		"a1 compute USD": "100", "a2 compute USD": "80",
		"(other) compute USD": "120", "(other) ai USD": "100", "(none) compute USD": "1000",
		// No EUR value has compute: all three rank last, by value.
		"a1 ai EUR": "1", "a3 ai EUR": "1", "(other) ai EUR": "9",
	}
	if !maps.Equal(split, wantSplit) {
		t.Errorf("rank=compute totals %v, want %v", split, wantSplit)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 3, "EUR": 1}) {
		t.Errorf("rank=compute otherCount %v", got.OtherCount)
	}

	// N at least the values: nothing folds, no otherCount.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&top=10"), &got); code != http.StatusOK || got.OtherCount != nil || len(got.Totals) != 9 {
		t.Errorf("top above values: %d %+v", code, got)
	}
	// Grouped by run, the names are the kept Runs' (one per currency), never (other).
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&group=family&top=1"), &got); code != http.StatusOK || len(got.Runs) != 2 || got.Runs[0].ID != "a5" || got.Runs[1].ID != "n1" {
		t.Errorf("run names: %d %+v", code, got.Runs)
	}

	for _, q := range []string{"&group=run&top=0", "&group=run&top=51", "&group=run&top=x", "&top=3", "&group=run&rank=compute", "&group=run&top=3&rank=ai"} {
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&top=50"), &got); code != http.StatusOK {
		t.Errorf("top=50: status %d", code)
	}
}

// 30 days per hour of 50 values in two families: unfolded, the breakdown
// passes the row limit; folded to 7 as the panel asks (the series per value
// with Show applied by family/nofamily, the family split as totals), it fits.
func TestCostSummaryTopUnderRowLimit(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from := t0.Add(-30 * 24 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels)
		SELECT 'v' || n, 't1', '{}', 'running', jsonb_build_object('app', 'v' || n) FROM generate_series(1, 50) n`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		SELECT $1::timestamptz + h * interval '1 hour', 't1', 'v' || n, f, f, 'USD', CASE f WHEN 'ai' THEN 51 - n ELSE n END, 0, 0
		FROM generate_series(0, 719) h, generate_series(1, 50) n, unnest(ARRAY['compute', 'ai']) f`, from)
	path := "/v1/costs?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&group=label:app"
	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], path+"&group=family&interval=hour", &got); code != http.StatusRequestEntityTooLarge {
		t.Errorf("unfolded: status %d, want 413", code)
	}
	for _, show := range []struct{ q, rank, first string }{
		{"", "all", "v1"}, // every value costs 51 per hour: ties by value
		{"&family=compute", "compute", "v50"},
		{"&nofamily=compute", "external", "v1"},
	} {
		got = CostSummaryBody{}
		if code := getJSON(t, s, keys["t1"], path+show.q+"&interval=hour&top=7&rank="+show.rank, &got); code != http.StatusOK {
			t.Fatalf("%s series: status %d", show.rank, code)
		}
		// 7 values and (other), every hour.
		if len(got.Series) != 720*8 || len(got.Totals) != 8 || got.OtherCount["USD"] != 43 {
			t.Errorf("%s series: %d rows, %d totals, otherCount %v", show.rank, len(got.Series), len(got.Totals), got.OtherCount)
		}
		if _, ok := byGroup(got.Totals, "label:app")[show.first+" USD"]; !ok {
			t.Errorf("%s: %s not kept: %v", show.rank, show.first, byGroup(got.Totals, "label:app"))
		}
		split := CostSummaryBody{}
		if code := getJSON(t, s, keys["t1"], path+"&group=family&top=7&rank="+show.rank, &split); code != http.StatusOK || len(split.Totals) != 16 {
			t.Errorf("%s split: %d %d rows", show.rank, code, len(split.Totals))
		}
		// Both calls keep the same values.
		for _, r := range got.Totals {
			if !slices.ContainsFunc(split.Totals, func(x CostSummaryRow) bool { return x.Group["label:app"] == r.Group["label:app"] }) {
				t.Errorf("%s: %s in the series call, not in the split", show.rank, r.Group["label:app"])
			}
		}
	}
	// One family only, the folded series with its family split fits too.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], path+"&group=family&family=compute&interval=hour&top=7", &got); code != http.StatusOK || len(got.Series) != 720*8 {
		t.Errorf("one family folded: %d %d", code, len(got.Series))
	}
}
