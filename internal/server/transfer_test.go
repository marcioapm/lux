package server

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// transferHost adds t1's provider host "nh" in eu-north-1 (pool "ec2p" of
// provider ec2), billed from 10:00 at $0.40/h with no volumes, and its
// network (partial or not).
func transferHost(t *testing.T, s *Server, partial bool) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	network := `{"subnetCidrs":["10.0.0.0/24"],"zone":"eu-north-1a","vpcSubnets":[],"publicIPv4":1}`
	if partial {
		network = `{"partial":true,"subnetCidrs":[],"zone":"eu-north-1a","vpcSubnets":[],"publicIPv4":1}`
	}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, instance_type, market, zone,
			launch_template, volumes, network)
		VALUES ('nh', 't1', 'nh', 'ec2p', 'ready', $1, $1, 'm7i.2xlarge', 'on-demand', 'eu-north-1a', '{"region":"eu-north-1"}', '[]', $2)`, at("10:00"), network)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('nh', $1, 0.40, 'USD', 8, $2, 'ec2-pricing')`, at("10:00"), 32*gib)
}

// cacheTransfer stores eu-north-1's prices as the price loop would.
func cacheTransfer(t *testing.T, s *Server) {
	t.Helper()
	p := &fakePriceProvider{transfer: map[string]DataTransferPrices{"eu-north-1": eun1Transfer()}}
	s.cfg.Costs.Prices = map[string]PriceProvider{"ec2": p}
	s.cfg.Costs.PricesRefresh = time.Hour
	if _, err := s.dataTransferPrice(context.Background(), "ec2", "eu-north-1"); err != nil {
		t.Fatal(err)
	}
}

func setNet(t *testing.T, s *Server, placement string, net map[string]proto.NetBytes) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE placements SET net_classes = $2 WHERE id = $1`, placement, net)
}

// transferLinesOf is a Run's data-transfer lines as "item amount final".
func transferLinesOf(t *testing.T, s *Server, key, runID string) ([]string, RunCost) {
	t.Helper()
	code, c := getCost(t, s, key, runID)
	if code != http.StatusOK {
		t.Fatalf("GET cost: %d", code)
	}
	var out []string
	for _, l := range c.Lines {
		if l.Family == familyDataTransfer {
			out = append(out, fmt.Sprintf("%s %s %v", l.Item, l.Amount, l.Final))
		}
	}
	return out, c
}

const gb = 1_000_000_000

// Each billable class and direction is bytes × its per-GB price, GB being
// 10^9 bytes; inbound is priced by what was delivered (rxDelivered), not
// what the filters accepted. Free classes (local, region-service,
// provider-global-free) produce no line; provider-global bills as internet.
func TestTransferSnapshotPerClass(t *testing.T) {
	s, keys := costFixture(t)
	transferHost(t, s, false)
	cacheTransfer(t, s)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	setNet(t, s, "p-A", map[string]proto.NetBytes{
		"internet":             {Tx: 3 * gb, Rx: 1 * gb, RxDelivered: 2 * gb}, // inbound from the internet is free
		"zone":                 {Tx: gb / 2, Rx: gb / 4, RxDelivered: gb},     // both ways at 0.01
		"region-public":        {Tx: 2 * gb},
		"provider:eu-west-1":   {Tx: 5 * gb, Rx: 9 * gb}, // inter-region inbound is free
		"provider-global":      {Tx: gb},                 // Route 53 et al.: internet out
		"provider-global-free": {Tx: 7 * gb},             // CloudFront
		"local":                {Tx: 100 * gb, Rx: 100 * gb},
		"region-service":       {Tx: 40 * gb},
	})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	want := []string{
		"internet:tx 0.27 true",          // 3 GB × 0.09
		"provider-global:tx 0.09 true",   // 1 GB × 0.09
		"provider:eu-west-1:tx 0.1 true", // 5 GB × 0.02
		"region-public:tx 0.02 true",     // 2 GB × 0.01
		"zone:rx 0.01 true",              // rxDelivered 1 GB × 0.01, not rx's 0.25 GB
		"zone:tx 0.005 true",             // 0.5 GB × 0.01
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %v\nwant  %v", lines, want)
	}
	if len(c.Unpriced) != 0 {
		t.Errorf("unpriced %v", c.Unpriced)
	}
	for _, l := range c.Lines {
		if l.Item == "zone:rx" {
			d := l.Details
			if d["class"] != "zone" || d["direction"] != "rx" || d["bytes"] != float64(gb) || d["perGB"] != "0.01" || d["currency"] != "USD" {
				t.Errorf("zone:rx details %v", d)
			}
		}
	}
	var total string
	for _, f := range c.ByFamily {
		if f.Family == familyDataTransfer {
			total = f.Amount + " " + f.DisplayName
		}
	}
	if total != "0.495 Data transfer" {
		t.Errorf("byFamily data-transfer %q", total)
	}
	var src string
	systemScan(t, s, `SELECT status FROM cost_sources WHERE run_id = 'A' AND source = 'compute'`, nil, &src)
	if src != "final" {
		t.Errorf("source %s", src)
	}
}

// unknown bytes produce no line and are reported as unpriced
// (unclassified); a host whose discovery was partial prices nothing and
// says why. Neither holds up finality: there is nothing a retry could
// price.
func TestTransferUnpriced(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			s, keys := costFixture(t)
			transferHost(t, s, partial)
			cacheTransfer(t, s)
			placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
			setNet(t, s, "p-A", map[string]proto.NetBytes{"unknown": {Tx: 4 * gb, Rx: 1, RxDelivered: 6}, "internet": {Tx: gb}, "local": {Tx: gb}})
			finish(t, s, "t1", "A", StateSucceeded)
			drain(t, s)
			lines, c := transferLinesOf(t, s, keys["t1"], "A")
			var unpriced []string
			for _, u := range c.Unpriced {
				unpriced = append(unpriced, fmt.Sprintf("%s %s rx%d tx%d", u.Class, u.Reason, u.Rx, u.Tx))
			}
			wantLines, wantUnpriced := []string{"internet:tx 0.09 true"}, []string{"unknown unclassified rx6 tx4000000000"}
			if partial {
				wantLines = nil
				wantUnpriced = []string{"internet partial-network rx0 tx1000000000", "unknown partial-network rx6 tx4000000000"}
			}
			if !slices.Equal(lines, wantLines) || !slices.Equal(unpriced, wantUnpriced) {
				t.Errorf("lines %v unpriced %v; want %v %v", lines, unpriced, wantLines, wantUnpriced)
			}
			for _, src := range c.Sources {
				if src.Source == "compute" && src.Status != "final" {
					t.Errorf("source %s", src.Status)
				}
			}
		})
	}
}

// No price cached for the host's region: billable bytes are missing, the
// source incomplete and retried, never zero; once the price is cached the
// retry prices and freezes it, and a later price change does not reprice.
func TestTransferMissingThenFinal(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	setNet(t, s, "p-A", map[string]proto.NetBytes{"internet": {Tx: 2 * gb}})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	if len(lines) != 0 || len(c.Unpriced) != 1 || c.Unpriced[0].Reason != unpricedNoPrice {
		t.Fatalf("no price: lines %v unpriced %v", lines, c.Unpriced)
	}
	var src, lastError string
	systemScan(t, s, `SELECT status, last_error FROM cost_sources WHERE run_id = 'A'`, nil, &src, &lastError)
	if src != "incomplete" || !strings.Contains(lastError, "no data-transfer price") {
		t.Fatalf("source %s %q", src, lastError)
	}
	cacheTransfer(t, s)
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'A'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if lines, c := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.18 true"}) || len(c.Unpriced) != 0 {
		t.Fatalf("priced: %v %v", lines, c.Unpriced)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET per_hour = 9 WHERE os = 'TRANSFER'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('A', 'retry')`)
	drain(t, s)
	if lines, _ := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.18 true"}) {
		t.Errorf("frozen snapshot repriced: %v", lines)
	}
}

// The placement's transfer goes to the hours its bytes moved in (sample
// deltas), not spread evenly over its window nor all in its last hour; the
// hours add up to the line.
func TestTransferHourlyFromSamples(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false)
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "13:30"))
	// 1 GB in the 10:00 hour, none in 11:00, 3 GB in 12:00, 1 GB unsampled
	// before the end (13:30).
	for _, smp := range []struct {
		at string
		tx int64
	}{{"10:20", gb}, {"10:50", gb}, {"12:10", 2 * gb}, {"12:40", 4 * gb}} {
		execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, net_classes) VALUES ('A', 1, 't1', 0, $1, $2)`,
			at(smp.at), map[string]proto.NetBytes{"internet": {Tx: smp.tx}})
	}
	setNet(t, s, "p-A", map[string]proto.NetBytes{"internet": {Tx: 5 * gb}})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	if lines, _ := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.45 true"}) {
		t.Fatalf("line %v", lines)
	}
	var hours string
	systemScan(t, s, `SELECT string_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || '=' || trim_scale(amount)::text, ' ' ORDER BY hour)
		FROM cost_hourly WHERE run_id = 'A' AND family = 'data-transfer'`, nil, &hours)
	if hours != "10=0.09 12=0.27 13=0.09" {
		t.Errorf("hours %s", hours)
	}
}

// transferHours' rounding keeps the hours' sum equal to the rounded total.
func TestTransferHoursSumToTotal(t *testing.T) {
	p := eun1TransferTrimmed()
	var samples []netSample
	for i := range 7 {
		samples = append(samples, netSample{At: at("10:00").Add(time.Duration(i) * 50 * time.Minute),
			Net: map[string]proto.NetBytes{"zone": {Tx: int64(i+1) * 333_333_333, RxDelivered: int64(i) * 111_111_111}}})
	}
	final := map[string]proto.NetBytes{"zone": {Tx: 7*333_333_333 + 5, RxDelivered: 6*111_111_111 + 9}}
	hours := transferHours(samples, final, at("16:00"), &p)
	sum := new(big.Rat)
	for _, h := range hours {
		sum.Add(sum, mustRat(h.Amount))
	}
	_, _, total := priceTransfer(final, &p, false)
	if moneyString(sum) != moneyString(total) {
		t.Errorf("hours sum %s, total %s (%v)", moneyString(sum), moneyString(total), hours)
	}
}
