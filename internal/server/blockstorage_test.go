package server

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// eun1GP3 is AWS's eu-north-1 gp3 list price (Pricing API, October 2026).
var eun1GP3 = BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0836", PerIOPSMonth: "0.0052", PerGiBpsMonth: "42.8032"}

// A volume's hourly price: storage always; gp3's IOPS and throughput only
// above 3000 and 125 MiB/s; io1/io2 every IOPS; gp2 storage only; the
// throughput price is per GiB/s, so 1024 MiB/s cost one unit; a month is
// 730 hours.
func TestVolumeHourly(t *testing.T) {
	io2 := BlockStoragePrice{Currency: "USD", PerGBMonth: "0.125", PerIOPSMonth: "0.065"}
	gp2 := BlockStoragePrice{Currency: "USD", PerGBMonth: "0.1"}
	for _, c := range []struct {
		name  string
		v     HostVolume
		price BlockStoragePrice
		month string // the expected monthly price; hourly is it / 730
	}{
		{"gp3 baseline", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, eun1GP3, "8.36"},
		{"gp3 below baseline", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 1000, ThroughputMiBps: 0}, eun1GP3, "8.36"},
		{"gp3 extra IOPS", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 4000, ThroughputMiBps: 125}, eun1GP3, "13.56"},
		// 1024 MiB/s above the baseline is one GiB/s-month: 42.8032.
		{"gp3 extra throughput", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 1149}, eun1GP3, "51.1632"},
		{"gp3 both", HostVolume{Type: "gp3", SizeGiB: 10, IOPS: 3500, ThroughputMiBps: 253}, eun1GP3, "8.7864"},
		{"io2 every IOPS", HostVolume{Type: "io2", SizeGiB: 100, IOPS: 1000}, io2, "77.5"},
		{"io1 every IOPS", HostVolume{Type: "io1", SizeGiB: 100, IOPS: 1000}, io2, "77.5"},
		{"gp2 storage only", HostVolume{Type: "gp2", SizeGiB: 100, IOPS: 300}, gp2, "10"},
		{"st1 storage only", HostVolume{Type: "st1", SizeGiB: 500, ThroughputMiBps: 500}, gp2, "50"},
	} {
		got, err := volumeHourly(c.v, c.price)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := new(big.Rat).Quo(mustRat(c.month), big.NewRat(730, 1))
		if got.Cmp(want) != 0 {
			t.Errorf("%s: %s/h, want %s/730 = %s", c.name, got.FloatString(12), c.month, want.FloatString(12))
		}
	}
	// gp3 100 GiB at the baseline in eu-north-1: 8.36 / 730 = 0.011452055 per hour.
	got, _ := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, eun1GP3)
	if moneyString(got) != "0.011452055" {
		t.Errorf("gp3 100GiB: %s", moneyString(got))
	}
}

// What cannot be priced is an error, never zero: an unknown type, or a
// dimension the type bills without a price.
func TestVolumeHourlyRefusesUnpriced(t *testing.T) {
	if _, err := volumeHourly(HostVolume{Type: "magnetic-x", SizeGiB: 1}, eun1GP3); !errors.Is(err, errUnknownVolumeType) {
		t.Errorf("unknown type: %v", err)
	}
	noIOPS := eun1GP3
	noIOPS.PerIOPSMonth = ""
	if _, err := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 1, IOPS: 4000}, noIOPS); err == nil {
		t.Error("gp3 above the IOPS baseline without an IOPS price")
	}
	if _, err := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 1, IOPS: 3000}, noIOPS); err != nil {
		t.Errorf("gp3 at the baseline needs no IOPS price: %v", err)
	}
}

// gp3eun1Answer is the Pricing API's gp3 eu-north-1 answer as ec2.Prices
// returns it (ten fractional digits).
var gp3eun1Answer = BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0836000000", PerIOPSMonth: "0.0052000000", PerGiBpsMonth: "42.8032000000"}

// The price refresh opens one block-storage period per provider host whose
// volumes are known: from its launch request to its end, at the sum of its
// volumes' hourly prices, with the volumes and unit prices it used. Hosts
// with unknown volumes, or a type that cannot be priced, get none (missing,
// not zero); compute periods are untouched; a second refresh changes nothing.
func TestBlockStorageRates(t *testing.T) {
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{{rate: HourlyRate{PerHour: "0.10", Currency: "USD"}}},
		blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "od", MarketOnDemand, from)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at, registered_at,
			instance_type, market, zone, launch_template, capacity, volumes) VALUES
		('unknown', 't1', 'unknown', 'pool-t1-ec2-burst', 'ready', 'i-u', $1, $1, 'm7i.large', 'on-demand', 'us-east-1a', '{"region":"us-east-1"}', '{"cpus":4}', NULL),
		('weird', 't1', 'weird', 'pool-t1-ec2-burst', 'ready', 'i-w', $1, $1, 'm7i.large', 'on-demand', 'us-east-1a', '{"region":"us-east-1"}', '{"cpus":4}',
			'[{"type":"floppy","sizeGiB":1}]')`, from)
	execSQL(t, s, ctx, `UPDATE hosts SET volumes = '[{"type":"gp3","sizeGiB":100,"iops":3000,"throughputMiBps":125},{"type":"gp3","sizeGiB":50,"iops":4000,"throughputMiBps":125}]' WHERE id = 'od'`)

	s.refreshPrices(ctx)
	type rate struct {
		Family, PerHour, Source string
		From                    time.Time
		Open                    bool
		CPUs                    float64
		Details                 *string
	}
	rates := func(host string) []rate {
		var out []rate
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT family, trim_scale(per_hour)::text, source, valid_from, valid_to IS NULL, cap_cpus, details::text
				FROM host_rates WHERE host_id = $1 ORDER BY family, valid_from`, host)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[rate])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := rates("od")
	// 100 GiB × 0.0836 + 50 GiB × 0.0836 + 1000 IOPS × 0.0052 = 17.74 a month; ÷ 730.
	if len(got) != 2 || got[0].Family != "block-storage" || got[0].PerHour != "0.02430137" || !got[0].From.Equal(from) ||
		!got[0].Open || got[0].Source != "ec2-ebs-pricing" || got[0].CPUs != 4 || got[1].Family != "compute" || got[1].PerHour != "0.1" {
		t.Fatalf("rates: %+v", got)
	}
	var d struct {
		Volumes []HostVolume
		Prices  map[string]map[string]string
	}
	if err := json.Unmarshal([]byte(*got[0].Details), &d); err != nil || len(d.Volumes) != 2 || d.Prices["gp3"]["perGiBpsMonth"] != "42.8032" {
		t.Errorf("details %s (%v)", *got[0].Details, err)
	}
	if r := rates("unknown"); len(r) != 1 || r[0].Family != "compute" {
		t.Errorf("unknown volumes priced: %+v", r)
	}
	if r := rates("weird"); len(r) != 1 || r[0].Family != "compute" {
		t.Errorf("unpriceable type priced: %+v", r)
	}
	s.refreshPrices(ctx)
	if again := rates("od"); len(again) != 2 || again[0].PerHour != got[0].PerHour || !again[0].From.Equal(got[0].From) || *again[0].Details != *got[0].Details {
		t.Errorf("second refresh: %+v", again)
	}
}

// Unit prices are fetched once per (region, type) and cached in price_cache
// for costs.prices_refresh; a failed refresh keeps the stale cache; with
// nothing cached a failure is an error.
func TestBlockStoragePriceCache(t *testing.T) {
	p := &fakePriceProvider{blockStorage: map[string]BlockStoragePrice{
		"gp3": {Currency: "USD", PerGBMonth: "0.0836000000", PerIOPSMonth: "0.0052000000", PerGiBpsMonth: "42.8032000000"}}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3")
	if err != nil || got != eun1GP3 {
		t.Fatalf("first: %+v %v", got, err)
	}
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != eun1GP3 || len(p.blockStorageCalls) != 1 {
		t.Fatalf("cached: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	var rows int
	systemScan(t, s, `SELECT count(*) FROM price_cache WHERE os = 'EBS'`, nil, &rows)
	if rows != 3 {
		t.Errorf("%d cache rows, want 3", rows)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET fetched_at = now() - interval '2 hours'`)
	p.blockStorageErr = errors.New("throttled")
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != eun1GP3 || len(p.blockStorageCalls) != 2 {
		t.Errorf("stale after failure: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	if _, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "io2"); err == nil {
		t.Error("nothing cached and the provider failing: no error")
	}
}
