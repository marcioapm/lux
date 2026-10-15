package runner

import (
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"testing"

	"github.com/marcioapm/lux/internal/egress"
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

func TestHostRebootReportGrowthRestart(t *testing.T) {
	dir := t.TempDir()
	makeRunner := func(boot string) *Runner {
		r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
		if err := r.restoreHostNetwork(boot); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeRunner("old")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 100}})
	r.hostNetSeeded = true
	r.saveHostNetwork()
	r = makeRunner("new")
	r.welcomeNetwork(proto.Welcome{HostNet: map[string]proto.NetBytes{"internet": {Tx: 100}}})
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 20}})
	r.saveHostNetwork()
	if r.hostNet.value()["internet"].Tx != 120 {
		t.Fatal(r.hostNet.value())
	}
	// Ten unsent bytes survive in the kernel; only the prior sample is durable.
	r = makeRunner("new")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 30}})
	r.hostNet.reconcile(map[string]proto.NetBytes{"internet": {Tx: 120}})
	if r.hostNet.value()["internet"].Tx != 130 {
		t.Fatal(r.hostNet.value())
	}
}

func TestWelcomeFloorPersistedWithoutGrowth(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	p := &placement{r: r, runID: "run1", epoch: 2, phase: "running", dir: t.TempDir(), state: &runState{RunID: "run1", Epoch: 2}}
	p.observeNet(map[string]proto.NetBytes{"zone": {Tx: 10}})
	r.placements["run1"] = p
	r.welcomeNetwork(proto.Welcome{Live: []proto.LivePlacement{{RunID: "run1", Epoch: 2, Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Tx: 40}}}}}})
	state, err := readRunState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Net.value()["zone"].Tx != 40 {
		t.Fatal(state.Net.value())
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

// A host that rebooted (Unloaded keeps no counters, as after a reboot)
// starts its total from what luxd recorded; a reconnect does not add it
// again.
func TestWelcomeSeedsHostTrafficOnce(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	recorded := map[string]proto.NetBytes{"internet": {Rx: 500, Tx: 70}}
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	if got := r.hostNet.value(); !maps.Equal(got, recorded) {
		t.Errorf("after a reboot and a reconnect: %v, want %v", got, recorded)
	}
}

// A placement re-adopted from Live continues from what luxd recorded when
// that is more than the kernel's counters hold.
func TestWelcomeRaisesReadoptedPlacement(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	p := &placement{r: r, runID: "run1", epoch: 2, phase: "running"}
	p.netClasses.observe(map[string]proto.NetBytes{"zone": {Tx: 10}})
	r.placements["run1"] = p
	r.welcomeNetwork(proto.Welcome{Live: []proto.LivePlacement{{RunID: "run1", Epoch: 2, State: "running",
		Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Tx: 40}, "provider:eu-west-1": {Rx: 3}}}}}})
	want := map[string]proto.NetBytes{"zone": {Tx: 40}, "provider:eu-west-1": {Rx: 3}}
	if got := p.usage().Net; !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	p.netClasses.observe(map[string]proto.NetBytes{"zone": {Tx: 15}})
	if got := p.usage().Net["zone"].Tx; got != 45 {
		t.Errorf("after 5 more bytes: %d, want 45", got)
	}
}
