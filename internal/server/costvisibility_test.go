package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// costVisibilityFixture is poolFixture plus host-hour rows per family on
// tenant a's own pool (host ha) and the platform pool (hp), and an AI line
// with no host on a's Run bound to its own pool.
func costVisibilityFixture(t *testing.T) (*Server, map[string]string, time.Time) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	keys := poolFixture(t, s, ctx)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool_id, allocated, unallocated) VALUES
		($1, 'compute', 'block-storage', 'USD', 'hp', 'p-shared', 0.004, 0.002),
		($1, 'compute', 'compute', 'USD', 'ha', 'p-a', 0.30, 0.10),
		($1, 'compute', 'block-storage', 'USD', 'ha', 'p-a', 0.03, 0.01)`, hour)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount) VALUES
		($1, 'ta', 'ra-own', 'compute', 'block-storage', 'USD', 'ha', 'p-a', 0.03),
		($1, 'ta', 'ra-own', 'gateway', 'ai', 'USD', NULL, NULL, 7)`, hour)
	return s, keys, hour
}

func unallocatedOf(rows []CostSummaryRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Family+" "+r.Currency+" "+r.Amount)
	}
	slices.Sort(out)
	return fmt.Sprint(out)
}

func hostsOf(rows []HostAllocation) string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s %s %s/%s", r.HostID, r.Family, r.Allocated, r.Unallocated))
	}
	slices.Sort(out)
	return fmt.Sprint(out)
}

// /v1/costs: unallocated and by-host rows are split by family; a tenant sees
// its own pools' (compute and block storage), never a platform pool's; an
// operator narrowed to it sees the same; an operator over every tenant sees
// every host's. A label filter still drops them.
func TestCostSummaryUnallocatedPerFamilyAndTenant(t *testing.T) {
	s, keys, _ := costVisibilityFixture(t)
	for _, c := range []struct {
		key, query, unallocated, hosts string
	}{
		{"a", "", "[block-storage USD 0.01 compute USD 0.1]", "[ha block-storage 0.03/0.01 ha compute 0.3/0.1]"},
		{"op", "&tenant=a", "[block-storage USD 0.01 compute USD 0.1]", "[ha block-storage 0.03/0.01 ha compute 0.3/0.1]"},
		{"b", "", "[]", "[]"},
		{"op", "", "[block-storage USD 0.012 compute USD 0.35]",
			"[ha block-storage 0.03/0.01 ha compute 0.3/0.1 hp block-storage 0.004/0.002 hp compute 0.4/0.25]"},
	} {
		var body CostSummaryBody
		if code := getJSON(t, s, keys[c.key], "/v1/costs?since=6h&group=host"+c.query, &body); code != http.StatusOK {
			t.Fatalf("%s%s: %d", c.key, c.query, code)
		}
		if got := unallocatedOf(body.Unallocated); got != c.unallocated {
			t.Errorf("%s%s unallocated %s, want %s", c.key, c.query, got, c.unallocated)
		}
		if got := hostsOf(body.Hosts); got != c.hosts {
			t.Errorf("%s%s hosts %s, want %s", c.key, c.query, got, c.hosts)
		}
	}
	var filtered CostSummaryBody
	getJSON(t, s, keys["a"], "/v1/costs?since=6h&label=app=x", &filtered)
	if filtered.Unallocated != nil {
		t.Errorf("filtered summary carries unallocated %v", filtered.Unallocated)
	}
}

// Pool cost: a pool's machines only (an AI line of a Run bound to it is not
// counted), with its idle time per family to its owner tenant and to
// operators, never a platform pool's to a tenant.
func TestPoolCostMachinesOnlyAndIdleVisibility(t *testing.T) {
	s, keys, _ := costVisibilityFixture(t)
	var own costBody
	if code := getJSON(t, s, keys["a"], "/v1/pools/own/cost?since=6h", &own); code != http.StatusOK {
		t.Fatalf("a own: %d", code)
	}
	if got := money(own.Totals); fmt.Sprint(got) != "map[USD:1.03]" {
		t.Errorf("a's own pool totals %v, want compute 1 + block storage 0.03 and no AI", got)
	}
	if got := fmt.Sprint(idleOf(own.Idle)); got != "map[block-storage USD:0.01 compute USD:0.1]" {
		t.Errorf("a's own pool idle %s", got)
	}
	var fams []string
	for _, h := range own.Hosts {
		fams = append(fams, h.HostID+" "+h.Family)
	}
	if fmt.Sprint(fams) != "[ha block-storage ha compute]" || len(own.HostSeries) != 2 {
		t.Errorf("a's own pool hosts %v series %v", fams, own.HostSeries)
	}
	var shared costBody
	getJSON(t, s, keys["a"], "/v1/pools/shared/cost?owner=platform&since=6h", &shared)
	if len(shared.Idle) != 0 || len(shared.Hosts) != 0 || len(shared.HostSeries) != 0 {
		t.Errorf("a reads a platform pool's host time: %v %v", shared.Idle, shared.Hosts)
	}
	var op costBody
	getJSON(t, s, keys["op"], "/v1/pools/shared/cost?owner=platform&since=6h", &op)
	if got := fmt.Sprint(idleOf(op.Idle)); got != "map[block-storage USD:0.002 compute USD:0.25]" {
		t.Errorf("operator's platform idle %s", got)
	}
}

// Host cost: hours per family; the owner tenant of a host in its own pool
// reads its unallocated; rates carry their family (operators).
func TestHostCostPerFamily(t *testing.T) {
	s, keys, hour := costVisibilityFixture(t)
	execSQL(t, s, context.Background(), `INSERT INTO host_rates (host_id, family, valid_from, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('ha', 'compute', $1, 0.4, 'USD', 2, 100, 'static'), ('ha', 'block-storage', $1, 0.04, 'USD', 2, 100, 'ec2-ebs-pricing')`, hour)
	var c struct {
		Hours []hostCostHour
		Rates []hostCostRate
	}
	if code := getJSON(t, s, keys["a"], "/v1/hosts/ha/cost?since=6h", &c); code != http.StatusOK {
		t.Fatalf("a: %d", code)
	}
	var got []string
	for _, h := range c.Hours {
		u := "-"
		if h.Unallocated != nil {
			u = *h.Unallocated
		}
		got = append(got, h.Family+" "+h.Allocated+"/"+u)
	}
	if fmt.Sprint(got) != "[block-storage 0.03/0.01 compute 0.3/0.1]" || len(c.Rates) != 0 {
		t.Errorf("a's host hours %v rates %v", got, c.Rates)
	}
	c.Rates = nil
	getJSON(t, s, keys["op"], "/v1/hosts/ha/cost?since=6h", &c)
	var rates []string
	for _, r := range c.Rates {
		rates = append(rates, r.Family+" "+r.PerHour)
	}
	if fmt.Sprint(rates) != "[compute 0.4 block-storage 0.04]" {
		t.Errorf("operator rates %v", rates)
	}
}
