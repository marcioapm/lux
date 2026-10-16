package server

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Heartbeats sample the host's own counters and a placement's only when
// they changed since the last sample: unchanged ones leave the column
// NULL, so a reading the runner repeats adds no bytes twice.
func TestHeartbeatSamplesNetOnChange(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	beat := func(host, run map[string]proto.NetBytes) {
		t.Helper()
		if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Usage: &proto.HostUsage{Net: host},
			Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: &proto.Usage{Net: run}}}}); err != nil {
			t.Fatal(err)
		}
	}
	beat(map[string]proto.NetBytes{"internet": {Tx: 10}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	beat(map[string]proto.NetBytes{"internet": {Tx: 10}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	beat(map[string]proto.NetBytes{"internet": {Tx: 30}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	var hosts, placements []*string
	systemScan(t, s, `SELECT array_agg(net_classes->'internet'->>'tx' ORDER BY at) FROM host_samples WHERE host_id = 'h1' AND res = 0`, nil, &hosts)
	systemScan(t, s, `SELECT array_agg(net_classes->'zone'->>'tx' ORDER BY at) FROM placement_samples WHERE run_id = $1 AND res = 0`, []any{r1}, &placements)
	str := func(v []*string) []string {
		out := make([]string, len(v))
		for i, p := range v {
			out[i] = "-"
			if p != nil {
				out[i] = *p
			}
		}
		return out
	}
	if got := str(hosts); len(got) != 3 || got[0] != "10" || got[1] != "-" || got[2] != "30" {
		t.Errorf("host samples %v, want [10 - 30]", got)
	}
	if got := str(placements); len(got) != 3 || got[0] != "1" || got[1] != "-" || got[2] != "-" {
		t.Errorf("placement samples %v, want [1 - -]", got)
	}
}

// The host's own transfer becomes unallocated data-transfer rows, each
// hour its counters' growth in that hour at the host's prices; over the
// host's life they add up to its cumulative counters priced at once.
func TestHostTransferHours(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	cacheTransfer(t, s)
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at,
			capacity, volumes, launch_template, network)
		VALUES ('ht', 't1', 'ht', 'ec2p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', '{"region":"eu-north-1"}', '{"publicIPv4":0}')`,
		hour, hour.Add(3*time.Hour))
	// Cumulative: hour 0 reaches 2 GB zone tx; hour 1 nothing new; hour 2
	// reaches 5 GB zone tx and 1 GB internet tx, 3 GB delivered from zone.
	// The first hour's last reading is also in a minute rollup: the same
	// counters at another resolution add nothing.
	for _, smp := range []struct {
		min, res int
		net      map[string]proto.NetBytes
	}{
		{10, 0, map[string]proto.NetBytes{"zone": {Tx: gb}}},
		{50, 0, map[string]proto.NetBytes{"zone": {Tx: 2 * gb}}},
		{50, 60, map[string]proto.NetBytes{"zone": {Tx: 2 * gb}}},
		{130, 0, map[string]proto.NetBytes{"zone": {Tx: 4 * gb}, "internet": {Tx: gb}}},
		{170, 0, map[string]proto.NetBytes{"zone": {Tx: 5 * gb, Rx: gb, RxDelivered: 3 * gb}, "internet": {Tx: gb}, "local": {Tx: 50 * gb}}},
	} {
		execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, net_classes) VALUES ('ht', $1, $2, $3)`,
			smp.res, hour.Add(time.Duration(smp.min)*time.Minute), smp.net)
	}
	for range 4 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var rows []string
	systemScan(t, s, `SELECT coalesce(array_agg(extract(hour FROM hour - $1::timestamptz)::text || '=' || trim_scale(unallocated)::text || '/' || trim_scale(allocated)::text ORDER BY hour), '{}')
		FROM cost_hourly WHERE host_id = 'ht' AND run_id IS NULL AND family = 'data-transfer'`, []any{hour}, &rows)
	// Hour 0: 2 GB zone tx × 0.01. Hour 2: 3 GB zone tx × 0.01 + 3 GB zone
	// delivered × 0.01 + 1 GB internet × 0.09. Local is free.
	want := []string{"0=0.02/0", "2=0.15/0"}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("rows %v, want %v", rows, want)
	}
	p := eun1TransferTrimmed()
	_, _, total := priceTransfer(map[string]proto.NetBytes{"zone": {Tx: 5 * gb, Rx: gb, RxDelivered: 3 * gb}, "internet": {Tx: gb}, "local": {Tx: 50 * gb}}, &p, false)
	if sum := new(big.Rat).Add(mustRat("0.02"), mustRat("0.15")); sum.Cmp(total) != 0 {
		t.Errorf("hours sum %s, cumulative priced %s", sum.FloatString(9), total.FloatString(9))
	}
}
