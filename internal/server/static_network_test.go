package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// A static pool's transfer prices are copied to a host registering into
// it; PUT /v1/hosts/{id}/network sets its catalog (its runner's classes,
// Source static) and replaces its prices; a provider's host is refused.
func TestStaticHostNetworkSettings(t *testing.T) {
	s, keys := priceFixture(t)
	prices := &DataTransferPrices{Currency: "USD", Internet: PerGB{Tx: "0.0010"}, Zone: PerGB{Rx: "0.002", Tx: "0.002"}}
	if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "lab", Provider: "static", TransferPrices: prices}); code != http.StatusOK {
		t.Fatalf("put pool: %d %s", code, body)
	}
	if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "burst", Provider: "ec2", TransferPrices: prices}); code != http.StatusUnprocessableEntity {
		t.Errorf("ec2 pool with transfer prices: %d %s", code, body)
	}
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	var h Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/"+h1, &h); code != http.StatusOK {
		t.Fatalf("get host %d", code)
	}
	if h.TransferPrices == nil || h.TransferPrices.Internet.Tx != "0.001" || h.StaticNetwork != nil {
		t.Fatalf("copied prices %+v network %+v", h.TransferPrices, h.StaticNetwork)
	}
	w, err := s.registerHost(context.Background(), &hostToken{ID: "tok1", TenantID: strp("t1"), PoolID: strp(poolID(t, s, "lab", "t1"))},
		proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", Capacity: proto.Capacity{CPUs: 8, Memory: 32 * gib, Runs: 4}})
	if err != nil || w.Network != nil {
		t.Fatalf("no catalog: classes %+v %v", w.Network, err)
	}

	settings := HostNetworkSettings{Network: &StaticNetwork{Local: []string{"10.1.0.0/24"}, Zone: []string{"10.1.0.0/16"}},
		TransferPrices: &DataTransferPrices{Currency: "USD", Internet: PerGB{Tx: "0.002"}, Zone: PerGB{Rx: "0.01", Tx: "0.01"}, PublicIPv4PerHour: "0.0050"}}
	code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/network", settings)
	if code != http.StatusOK {
		t.Fatalf("set network: %d %s", code, body)
	}
	w, err = s.registerHost(context.Background(), &hostToken{ID: "tok1", TenantID: strp("t1"), PoolID: strp(poolID(t, s, "lab", "t1"))},
		proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", Capacity: proto.Capacity{CPUs: 8, Memory: 32 * gib, Runs: 4}})
	if err != nil || w.Network == nil || w.Network.Source != proto.NetSourceStatic || !w.Network.Catalog() ||
		!slices.Equal(w.Network.Local, []string{"10.1.0.0/24"}) || !slices.Equal(w.Network.Zone, []string{"10.1.0.0/16"}) || w.Network.Version == "" {
		t.Fatalf("classes after set: %+v %v", w.Network, err)
	}
	if got := periods(t, s, h1); !slices.Equal(got, []string{"0.005 USD 8 32 open"}) {
		t.Errorf("public-ip period %v", got)
	}
	for _, bad := range []HostNetworkSettings{
		{Network: &StaticNetwork{Local: []string{"10.1.0.0/33"}}},
		{TransferPrices: &DataTransferPrices{Currency: "usd"}},
		{TransferPrices: &DataTransferPrices{Currency: "USD", Internet: PerGB{Tx: "-1"}}},
		{TransferPrices: &DataTransferPrices{Currency: "USD", Provider: map[string]PerGB{"Bad Region": {Tx: "1"}}}},
	} {
		if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/network", bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%+v: %d %s, want 422", bad, code, body)
		}
	}
	if code, _ := call(t, s, keys["t2"], http.MethodPut, "/v1/hosts/h1/network", settings); code != http.StatusNotFound {
		t.Errorf("another tenant's host: %d", code)
	}
	execSQL(t, s, context.Background(), `UPDATE hosts SET provision_requested_at = now() WHERE id = $1`, h1)
	if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/network", settings); code != http.StatusUnprocessableEntity {
		t.Errorf("provider host: %d %s", code, body)
	}
}

// A static host's transfer is priced at its operator's prices; one with no
// catalog has only local and unknown traffic, so nothing is priced and the
// unknown bytes are listed as unpriced; one without prices lists its
// billable classes as unpriced (no-price) without holding up finality,
// and so does one whose operator priced some classes but not others: the
// unpriced class is listed, never billed at zero.
func TestStaticHostTransferPricing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prices   *DataTransferPrices
		net      map[string]proto.NetBytes
		lines    []string
		unpriced []string
	}{
		{"priced", &DataTransferPrices{Currency: "EUR", Internet: PerGB{Tx: "0.001"}, Zone: PerGB{Rx: "0.01", Tx: "0.02"}},
			map[string]proto.NetBytes{"internet": {Tx: 10 * gb}, "zone": {Tx: gb, RxDelivered: 2 * gb}, "local": {Tx: gb}},
			[]string{"internet:tx 0.01 true", "zone:rx 0.02 true", "zone:tx 0.02 true"}, nil},
		{"no catalog", &DataTransferPrices{Currency: "EUR", Internet: PerGB{Tx: "0.001"}},
			map[string]proto.NetBytes{"local": {Tx: gb}, "unknown": {Tx: 3 * gb}},
			nil, []string{"unknown unclassified 0 3000000000"}},
		{"no prices", nil,
			map[string]proto.NetBytes{"internet": {Tx: gb}},
			nil, []string{"internet no-price 0 1000000000"}},
		{"class without a price", &DataTransferPrices{Currency: "EUR", Internet: PerGB{Tx: "0.001"}},
			map[string]proto.NetBytes{"internet": {Tx: gb}, "zone": {Tx: 7 * gb, RxDelivered: 3 * gb}},
			[]string{"internet:tx 0.001 true"}, []string{"zone no-price 3000000000 7000000000"}},
		{"explicit zero is free", &DataTransferPrices{Currency: "EUR", Internet: PerGB{Tx: "0.001"}, Zone: PerGB{Tx: "0"}},
			map[string]proto.NetBytes{"internet": {Tx: gb}, "zone": {Tx: 7 * gb, RxDelivered: 3 * gb}},
			[]string{"internet:tx 0.001 true"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, keys := costFixture(t)
			costHosts(t, s)
			if tc.prices != nil {
				execSQL(t, s, context.Background(), `UPDATE hosts SET transfer_prices = $1 WHERE id = 'static'`, tc.prices)
			}
			placeRun(t, s, "t1", "A", StateStopping, "static", place("A", 2, 8, "10:00", "10:30"))
			setNet(t, s, "p-A", tc.net)
			finish(t, s, "t1", "A", StateSucceeded)
			drain(t, s)
			lines, c := transferLinesOf(t, s, keys["t1"], "A")
			var unpriced []string
			for _, u := range c.Unpriced {
				unpriced = append(unpriced, fmt.Sprintf("%s %s %d %d", u.Class, u.Reason, u.Rx, u.Tx))
			}
			if !slices.Equal(lines, tc.lines) || !slices.Equal(unpriced, tc.unpriced) {
				t.Errorf("lines %v unpriced %v; want %v %v", lines, unpriced, tc.lines, tc.unpriced)
			}
			for _, src := range c.Sources {
				if src.Source == "compute" && src.Status != "final" {
					t.Errorf("source %s", src.Status)
				}
			}
		})
	}
}
