package server

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type storedPublicIP struct {
	From      time.Time
	To        *time.Time
	PerHour   string
	Addresses int
}

func publicIPRates(t *testing.T, s *Server, host string) []storedPublicIP {
	t.Helper()
	ctx := context.Background()
	var out []storedPublicIP
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT valid_from, valid_to, trim_scale(per_hour)::text, (details->>'addresses')::int
			FROM host_rates WHERE host_id = $1 AND family = 'public-ip' ORDER BY valid_from`, host)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[storedPublicIP])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A provider host's public-ip period opens from its registration once its
// addresses are known and its region's price is cached; a provider check
// that records another count closes it and opens one at the new count;
// zero addresses close it with none after.
func TestPublicIPPeriods(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 1) // 1 address, registered 10:00
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	s.refreshPublicIP(ctx) // no price cached and none to fetch: nothing
	if got := publicIPRates(t, s, "nh"); len(got) != 0 {
		t.Fatalf("opened without a price: %v", got)
	}
	cacheTransfer(t, s)
	s.refreshPublicIP(ctx)
	got := publicIPRates(t, s, "nh")
	if len(got) != 1 || !got[0].From.Equal(at("10:00")) || got[0].To != nil || got[0].PerHour != "0.005" || got[0].Addresses != 1 {
		t.Fatalf("first period %+v", got)
	}
	s.refreshPublicIP(ctx)
	if n := len(publicIPRates(t, s, "nh")); n != 1 {
		t.Fatalf("refresh reopened: %d periods", n)
	}
	store := func(addresses int) {
		t.Helper()
		n := hostA()
		n.Zone, n.PublicIPv4 = "eu-north-1a", addresses
		if err := s.storeNetworkRefresh(ctx, "nh", "ec2", "eu-north-1", n, catalogEU(at("09:00"))); err != nil {
			t.Fatal(err)
		}
	}
	store(1) // the same count, other subnets: the period stays
	if got := publicIPRates(t, s, "nh"); len(got) != 1 || got[0].To != nil {
		t.Fatalf("unchanged count reopened: %+v", got)
	}
	store(3)
	got = publicIPRates(t, s, "nh")
	if len(got) != 2 || got[0].To == nil || !got[0].To.Equal(got[1].From) || got[1].To != nil || got[1].PerHour != "0.015" || got[1].Addresses != 3 {
		t.Fatalf("after 3 addresses: %+v", got)
	}
	store(0)
	got = publicIPRates(t, s, "nh")
	if len(got) != 2 || got[1].To == nil {
		t.Fatalf("after 0 addresses: %+v", got)
	}
	store(2)
	if got := publicIPRates(t, s, "nh"); len(got) != 3 || got[2].PerHour != "0.01" || got[2].To != nil {
		t.Fatalf("after 2 addresses: %+v", got)
	}
}

// Host-hour rows: public IPv4 is split by the same shares as compute,
// allocated + unallocated = the addresses' cost of each hour.
func TestPublicIPHostHours(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at, capacity, volumes, network)
		VALUES ('hh', 't1', 'hh', 'p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', '{"publicIPv4": 2}')`, hour, hour.Add(2*time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('hh', $1, $2, 0.40, 'USD', 8, $3, 'aws-pricing')`, hour, hour.Add(2*time.Hour), 32*gib)
	// A: 2 of 8 CPUs over the first hour; B: 6 of 8 over its second half.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at) VALUES
		('p-hh', 't1', 'r1', 'hh', 1, 'exited', '{"cpus":2,"memory":8589934592}', $1, $2),
		('p-hh2', 't1', 'r1', 'hh', 2, 'exited', '{"cpus":6,"memory":8589934592}', $3, $2)`, hour, hour.Add(time.Hour), hour.Add(30*time.Minute))
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := syncPublicIPRate(ctx, tx, "hh", "0.005", "USD", "ec2-vpc-pricing", s.cfg.Costs.Hourly)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 2 addresses × 0.005 = 0.01 an hour. First hour: A 1/4 of it, B 3/4 of
	// its half (0.00375); second hour: all unallocated.
	h := func(n int) string { return hour.Add(time.Duration(n) * time.Hour).Format("15") }
	var rows []string
	systemScan(t, s, `SELECT coalesce(array_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || ' ' || trim_scale(allocated)::text || ' ' || trim_scale(unallocated)::text
		ORDER BY hour), '{}') FROM cost_hourly WHERE host_id = 'hh' AND run_id IS NULL AND family = 'public-ip'`, nil, &rows)
	want := []string{h(0) + " 0.00625 0.00375", h(1) + " 0 0.01"}
	if !slices.Equal(rows, want) {
		t.Errorf("public-ip host hours %v, want %v", rows, want)
	}
	for _, r := range rows {
		var hr string
		var a, u string
		fmt.Sscan(r, &hr, &a, &u)
		if sum := new(big.Rat).Add(mustRat(a), mustRat(u)); sum.Cmp(big.NewRat(1, 100)) != 0 {
			t.Errorf("%s: allocated + unallocated = %s, want the hour's 0.01", hr, sum.FloatString(9))
		}
	}
}

// A placement on a host with addresses gets a public-ip line from the same
// shares as compute; the Run is final only once it is priced.
func TestPublicIPRunLine(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 1)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, src := familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 false]" || src != "incomplete" {
		t.Fatalf("before the period: %v %s", lines, src)
	}
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	s.refreshPublicIP(ctx) // re-queues A
	drain(t, s)
	// 1 address × 0.005 for an hour, a quarter share: 0.00125.
	lines, src = familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 true public-ip ipv4 0.00125 true]" || src != "final" {
		t.Errorf("priced: %v %s", lines, src)
	}
}
