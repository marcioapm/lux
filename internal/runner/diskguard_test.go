package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/runner/disklimit"
	"github.com/marcioapm/lux/internal/spec"
)

const mib = int64(1) << 20

// fakeFS drives a diskGuard: free is what the next measurements answer
// (the last value repeats), and every guard action is logged in order.
type fakeFS struct {
	frees    []int64
	measured int
	calls    []string
	reclaims bool // what reclaim answers
	victims  []diskVictim
	stopped  []string
	// onStop, when set, runs after each stop (the filesystem changing
	// between two victims).
	onStop func(f *fakeFS)
}

func (f *fakeFS) free() (int64, error) {
	i := min(f.measured, len(f.frees)-1)
	f.measured++
	return f.frees[i], nil
}

// guard is a guard over a 1000 MiB filesystem, low 50 MiB, recover 100
// MiB, acting on f.
func (f *fakeFS) guard(t *testing.T) *diskGuard {
	t.Helper()
	g, err := newDiskGuard(slog.New(slog.NewTextHandler(io.Discard, nil)), FreeMark{Bytes: 50 * mib}, FreeMark{Bytes: 100 * mib}, 1000*mib)
	if err != nil {
		t.Fatal(err)
	}
	g.free = f.free
	g.publish = func(context.Context) {
		state := "clear"
		if p := g.state(); p != nil {
			state = fmt.Sprintf("pressure noRelief=%v", p.NoRelief)
		}
		f.calls = append(f.calls, "publish "+state)
	}
	g.reclaim = func(_ context.Context, need int64) bool {
		f.calls = append(f.calls, fmt.Sprintf("reclaim %d MiB", need/mib))
		return f.reclaims
	}
	g.victims = func() []diskVictim { return f.victims }
	g.stop = func(_ context.Context, v diskVictim, _ int64) {
		f.calls = append(f.calls, "stop "+v.runID)
		f.stopped = append(f.stopped, v.runID)
		if f.onStop != nil {
			f.onStop(f)
		}
	}
	return g
}

// victim is a placement held to limit MiB that used used MiB.
func victim(id string, used, limit int64) diskVictim {
	return diskVictim{runID: id, used: used * mib, limit: disklimit.Limit{Nominal: limit * mib, Effective: limit * mib}}
}

// Pressure starts only below the low mark, holds between the marks, and
// ends only at the recovery mark; each change is published at once.
func TestDiskGuardHysteresis(t *testing.T) {
	f := &fakeFS{}
	g := f.guard(t)
	for _, step := range []struct {
		free     int64
		pressure bool
	}{
		{200, false},
		{50, false}, // at the low mark is not below it
		{49, true},  // below: pressure starts
		{60, true},  // above low, below recover: holds
		{99, true},
		{100, false}, // at the recovery mark: ends
		{60, false},  // between the marks again: no pressure until below low
		{10, true},
	} {
		f.frees = []int64{step.free * mib}
		g.pass(context.Background())
		if got := g.state() != nil; got != step.pressure {
			t.Fatalf("free %d MiB: pressure %v, want %v", step.free, got, step.pressure)
		}
		if p := g.state(); p != nil && p.FreeBytes != step.free*mib {
			t.Errorf("free %d MiB: state says %d", step.free, p.FreeBytes)
		}
	}
	want := []string{"publish pressure noRelief=false", "publish clear", "publish pressure noRelief=false"}
	var published []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "publish") {
			published = append(published, c)
		}
	}
	// noRelief flips to true and back as passes find nothing: count only
	// the transitions of pressure itself here.
	published = slices.DeleteFunc(published, func(s string) bool { return s == "publish pressure noRelief=true" })
	if !slices.Equal(published, want) {
		t.Errorf("published %v, want %v", published, want)
	}
	p := g.state()
	if p.LowFreeBytes != 50*mib || p.RecoverFreeBytes != 100*mib || p.LowFree != "50.0 MiB" || p.RecoverFree != "100.0 MiB" {
		t.Errorf("state %+v", p)
	}
}

// On entering pressure the guard blocks admissions first (publishes),
// then reclaims caches toward the recovery mark, then stops over-limit
// placements; in that order, on the same pass.
func TestDiskGuardOrderBlockReclaimStop(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib}, reclaims: true, victims: []diskVictim{victim("over", 300, 100)}}
	f.guard(t).pass(context.Background())
	want := []string{"publish pressure noRelief=false", "reclaim 60 MiB", "stop over"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
}

// Reclaim that reaches the recovery mark stops nothing: the filesystem is
// measured again after it.
func TestDiskGuardReclaimEnoughStopsNothing(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib, 120 * mib}, reclaims: true, victims: []diskVictim{victim("over", 300, 100)}}
	f.guard(t).pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v after reclaiming to the recovery mark", f.stopped)
	}
}

// Victims are taken largest excess first; after each the filesystem is
// measured again, and the guard stops only as many as it still lacks.
func TestDiskGuardLargestExcessFirstWithRemeasure(t *testing.T) {
	vs := []diskVictim{victim("small", 110, 100), victim("large", 400, 100), victim("mid", 150, 100)}
	t.Run("excess alone", func(t *testing.T) {
		// 60 MiB short of recover: large's 300 MiB covers it.
		f := &fakeFS{frees: []int64{40 * mib}, victims: vs}
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large"}) {
			t.Fatalf("stopped %v, want [large]", f.stopped)
		}
	})
	t.Run("in order of excess", func(t *testing.T) {
		// Excesses 300, 50, 10 MiB do not cover a 400 MiB shortfall:
		// every one goes, largest first.
		f := &fakeFS{frees: []int64{-300 * mib}, victims: vs}
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large", "mid", "small"}) {
			t.Fatalf("stopped %v, want [large mid small]", f.stopped)
		}
	})
	t.Run("measured again between victims", func(t *testing.T) {
		// 400 MiB short; after the first stop the filesystem has freed
		// enough (another Run ended): no second victim.
		f := &fakeFS{frees: []int64{-300 * mib}, victims: vs}
		f.onStop = func(f *fakeFS) { f.frees = []int64{150 * mib} }
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large"}) {
			t.Fatalf("stopped %v, want [large]: the second measurement shows enough free", f.stopped)
		}
	})
}

// A placement already stopping counts toward what is needed: with its
// excess covering the shortfall, no other placement is stopped; with it
// short, only what remains.
func TestDiskGuardCountsPlacementsAlreadyStopping(t *testing.T) {
	going := victim("going", 300, 100)
	going.stopping = true
	f := &fakeFS{frees: []int64{40 * mib}, victims: []diskVictim{going, victim("other", 400, 100)}}
	g := f.guard(t)
	g.pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v while a stopping placement's excess covers the need", f.stopped)
	}
	if g.state().NoRelief {
		t.Error("a stopping placement is relief, reported as none")
	}
	going.used = 120 * mib // 20 MiB excess, 40 MiB still lacking
	f.victims = []diskVictim{going, victim("other", 400, 100), victim("third", 200, 100)}
	g.pass(context.Background())
	if !slices.Equal(f.stopped, []string{"other"}) {
		t.Fatalf("stopped %v, want [other]", f.stopped)
	}
}

// A placement at or under its effective limit is never stopped, however
// low the disk; with nothing to reclaim and no over-limit placement, the
// pressure is reported unrelieved and admissions stay blocked.
func TestDiskGuardNeverStopsConformingAndReportsNoRelief(t *testing.T) {
	f := &fakeFS{frees: []int64{1 * mib}, victims: []diskVictim{victim("at", 100, 100), victim("under", 10, 100)}}
	g := f.guard(t)
	g.pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped conforming placements %v", f.stopped)
	}
	p := g.state()
	if p == nil || !p.NoRelief {
		t.Fatalf("state %+v, want pressure with no relief", p)
	}
	want := []string{"publish pressure noRelief=false", "reclaim 99 MiB", "publish pressure noRelief=true"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	// Still nothing on the next pass: blocked, not published again.
	g.pass(context.Background())
	if g.state() == nil || len(f.calls) != 4 || f.calls[3] != "reclaim 99 MiB" {
		t.Fatalf("second pass: state %+v calls %v", g.state(), f.calls)
	}
}

// Watermarks are refused unless 0 < low < recover < capacity, as resolved
// against the filesystem; same-unit marks are checked before it is read.
func TestDiskGuardMarks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range []struct {
		low, rec string
		capacity int64
		ok       bool
	}{
		{"5%", "10%", 100 * gib, true},
		{"8GiB", "16GiB", 100 * gib, true},
		{"5%", "16GiB", 100 * gib, true},
		{"10%", "5%", 100 * gib, false},
		{"5%", "5%", 100 * gib, false},
		{"8GiB", "8GiB", 100 * gib, false},
		{"8GiB", "5%", 100 * gib, false},     // 8 GiB is above 5 GiB
		{"1GiB", "200GiB", 100 * gib, false}, // recover past the capacity
		{"5%", "10%", 10, false},             // 0 bytes low
	} {
		low, err := ParseFreeMark(c.low)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := ParseFreeMark(c.rec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = newDiskGuard(logger, low, rec, c.capacity)
		if (err == nil) != c.ok {
			t.Errorf("low %s recover %s on %d bytes: err %v, want ok %v", c.low, c.rec, c.capacity, err, c.ok)
		}
	}
	for _, s := range []string{"0%", "100%", "-1%", "x%", "0", "-5", "lots", ""} {
		if m, err := ParseFreeMark(s); err == nil {
			t.Errorf("ParseFreeMark(%q) = %+v, want refused", s, m)
		}
	}
	if m, _ := ParseFreeMark("8GiB"); m.Bytes != 8*gib {
		t.Errorf("8GiB = %+v", m)
	}
	if m, _ := ParseFreeMark(" 2.5% "); m.Percent != 2.5 {
		t.Errorf("2.5%% = %+v", m)
	}
	if err := CheckFreeMarks(FreeMark{Percent: 10}, FreeMark{Percent: 5}); err == nil {
		t.Error("CheckFreeMarks 10% over 5%: accepted")
	}
}

// diskVictims lists only live placements whose latest sample is over
// their effective limit, marking those already on their way out.
func TestDiskVictimsOnlyOverTheLimit(t *testing.T) {
	f := newDiskFixture(t)
	f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib) // 10 GiB asked holds to 9.375
	const effective = diskLimit / 16 * 15
	add := func(id string, used int64, phase string) *placement {
		sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(diskLimit), DiskGrace: spec.SetDuration(diskGrace)}}
		p := newPlacement(f.r, proto.Assign{RunID: id, TenantID: "t1", Epoch: 1, Spec: sp}, nil)
		p.state = &runState{RunID: id, Epoch: 1}
		p.phase = phase
		p.lastDisk = used
		f.r.placements[id] = p
		return p
	}
	add("at", effective, "running")
	add("over", effective+1, "running")
	add("stopping", effective+5, "stopping")
	asked := add("asked", effective+7, "running")
	asked.stopWhy = "stop"
	add("nolimit", 1<<40, "running").assign.Spec.Resources.Disk = 0
	got := map[string]bool{}
	for _, v := range f.r.diskVictims() {
		got[v.runID] = v.stopping
		if v.limit.Effective != effective {
			t.Errorf("%s held to %d, want %d", v.runID, v.limit.Effective, effective)
		}
	}
	want := map[string]bool{"over": false, "stopping": true, "asked": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("victims %v, want %v", got, want)
	}
}

// pressureStop marks the placement (so the next pass counts it as
// stopping) and reports disk.pressure_stop with what it used and was held
// to, the excess and the filesystem's marks.
func TestPressureStopReportsToLuxd(t *testing.T) {
	f := newDiskFixture(t)
	f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib)
	g, err := newDiskGuard(f.r.log, DefaultDiskLowFree, DefaultDiskRecoverFree, 160*gib)
	if err != nil {
		t.Fatal(err)
	}
	f.r.attachGuard(g, func() (int64, error) { return gib, nil })
	p := f.placement(1)
	f.r.placements["run1"] = p
	p.lastDisk = 20 * gib
	vs := f.r.diskVictims()
	if len(vs) != 1 || vs[0].stopping {
		t.Fatalf("victims %+v", vs)
	}
	f.r.pressureStop(f.ctx, vs[0], gib)
	if vs := f.r.diskVictims(); len(vs) != 1 || !vs[0].stopping {
		t.Fatalf("after the stop: victims %+v, want it stopping", vs)
	}
	var ev proto.RunEvent
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if evs := f.runEvents(proto.EvDiskPressureStop); len(evs) > 0 {
			ev = evs[0]
			break
		}
	}
	effective := diskLimit / 16 * 15
	want := map[string]any{"usedBytes": float64(20 * gib), "limitBytes": float64(effective), "excessBytes": float64(20*gib - effective),
		"freeBytes": float64(gib), "lowFreeBytes": float64(8 * gib), "recoverFreeBytes": float64(16 * gib),
		"nominalLimitBytes": float64(diskLimit), "limitFactor": 0.9375}
	if fmt.Sprint(ev.Data) != fmt.Sprint(want) {
		t.Fatalf("event %+v, want data %v", ev, want)
	}
}

// podmanUsage is a host's usage with its filesystem's free bytes.
func podmanUsage() podman.HostUsage { return podman.HostUsage{DiskBytes: 1, DiskFree: 5, DiskOK: true} }

// A runner without disk scaling has no guard: its heartbeat never carries
// pressure, and New sets none up.
func TestNoGuardWithoutDiskScale(t *testing.T) {
	off := &Runner{cfg: Config{Disk: 160 * gib}}
	if u := off.hostUsage(podmanUsage()); u.DiskPressure != nil {
		t.Fatalf("legacy runner reported pressure %+v", u.DiskPressure)
	}
	off.guardPass(context.Background()) // no guard: nothing to do, no panic
	on := &Runner{cfg: Config{Disk: 160 * gib}, disk: mustDiskScale(t, 160*gib, 10*gib, 160*gib)}
	g, err := newDiskGuard(slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultDiskLowFree, DefaultDiskRecoverFree, 160*gib)
	if err != nil {
		t.Fatal(err)
	}
	on.guard = g
	g.observe(gib)
	u := on.hostUsage(podmanUsage())
	if u.DiskPressure == nil || u.DiskPressure.FreeBytes != gib || u.DiskPressure.LowFree != "5%" || u.DiskPressure.RecoverFree != "10%" {
		t.Fatalf("pressure %+v", u.DiskPressure)
	}
}
