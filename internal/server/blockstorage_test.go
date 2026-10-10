package server

import (
	"context"
	"errors"
	"math/big"
	"testing"
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
