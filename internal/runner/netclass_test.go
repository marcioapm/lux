package runner

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

func TestNetBaselineResetsAndReadoption(t *testing.T) {
	n := netBaseline{}
	sample := func(rx, tx int64) map[string]proto.NetBytes {
		return map[string]proto.NetBytes{"internet": {Rx: rx, Tx: tx}}
	}
	check := func(current, want map[string]proto.NetBytes) {
		t.Helper()
		got := n.observe(current)
		if !maps.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	check(sample(100, 200), sample(100, 200))
	check(sample(120, 250), sample(120, 250))
	check(sample(5, 10), sample(125, 260))
	check(sample(10, 20), sample(130, 270))
	// Baseline survives restart; surviving kernel counters are not added twice.
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	n = netBaseline{}
	if err = json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	check(sample(15, 30), sample(135, 280))
	n.reconcile(sample(140, 290))
	check(sample(20, 40), sample(145, 300))
	// Reboot: kernel gone; fold the last reading in before new bytes.
	check(nil, sample(145, 300))
	check(sample(3, 4), sample(148, 304))
}

func TestNetWelcomeFloorDoesNotDoubleCount(t *testing.T) {
	n := netBaseline{}
	n.observe(map[string]proto.NetBytes{"provider:eu-west-1": {Tx: 100}})
	n.reconcile(map[string]proto.NetBytes{"provider:eu-west-1": {Tx: 90}, "unknown": {Rx: 7}})
	got := n.value()
	if got["provider:eu-west-1"].Tx != 100 || got["unknown"].Rx != 7 {
		t.Fatal(got)
	}
	n.reconcile(got)
	if !maps.Equal(n.value(), got) {
		t.Fatal("repeated welcome added twice")
	}
}

func TestUsageNetCompatibility(t *testing.T) {
	raw, err := json.Marshal(proto.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["net"]; ok {
		t.Fatal("empty net not omitted")
	}
	want := proto.Usage{Net: map[string]proto.NetBytes{"unknown": {Rx: 3, Tx: 4}, "provider:eu-west-1": {Tx: 9}}}
	raw, err = json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got proto.Usage
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Net, want.Net) {
		t.Fatal(got.Net)
	}
}
