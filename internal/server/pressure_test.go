package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Heartbeats raise a placement's peak CPU and stall totals and never lower
// them; a heartbeat without them (an older runner, or no PSI) leaves NULL,
// not 0; the exit report's smaller values change nothing; the Run's JSON
// carries them per placement and rolled up.
func TestPeakCPUAndStallsOnPlacement(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	beat := func(u *proto.Usage) {
		t.Helper()
		if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: u}}}); err != nil {
			t.Fatal(err)
		}
	}
	type stored struct{ peak, cpu, mem, memFull, io, ioFull *float64 }
	read := func() stored {
		t.Helper()
		var x stored
		row := queryOne[[]*float64](t, s, `SELECT ARRAY[peak_cpu_cores, cpu_stall_seconds, memory_stall_seconds, memory_full_stall_seconds,
			io_stall_seconds, io_full_stall_seconds] FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
		x.peak, x.cpu, x.mem, x.memFull, x.io, x.ioFull = row[0], row[1], row[2], row[3], row[4], row[5]
		return x
	}

	beat(&proto.Usage{CPUSeconds: 5, PeakMemoryBytes: 1 << 20})
	if x := read(); x.peak != nil || x.cpu != nil || x.mem != nil || x.memFull != nil || x.io != nil || x.ioFull != nil {
		t.Fatalf("an older runner's heartbeat stored %+v, want every field NULL", x)
	}

	beat(&proto.Usage{CPUSeconds: 10, PeakCPUCores: f64(1.5), CPUCores: f64(1.5), CPUStallSeconds: f64(2), MemoryStallSeconds: f64(0),
		MemoryFullStallSeconds: f64(0), IOStallSeconds: f64(4), IOFullStallSeconds: f64(1)})
	beat(&proto.Usage{CPUSeconds: 20, PeakCPUCores: f64(3.25), CPUCores: f64(3.25), CPUStallSeconds: f64(6), MemoryStallSeconds: f64(0.5),
		MemoryFullStallSeconds: f64(0.25), IOStallSeconds: f64(4), IOFullStallSeconds: f64(1)})
	stopUsageRun(t, s, ctx, key)
	code := 0
	if err := reportUsageStatus(s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped",
		Usage: &proto.Usage{PeakCPUCores: f64(1), CPUStallSeconds: f64(1)}}); err != nil {
		t.Fatal(err)
	}
	x := read()
	got := fmt.Sprint(*x.peak, *x.cpu, *x.mem, *x.memFull, *x.io, *x.ioFull)
	if want := fmt.Sprint(3.25, 6, 0.5, 0.25, 4, 1); got != want {
		t.Fatalf("stored peak and stalls %s, want %s", got, want)
	}

	// A second placement on the Run: the Run's peak is the maximum, its
	// stalls the sum; a placement without them adds nothing.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, peak_cpu_cores, cpu_stall_seconds)
		VALUES ('p2', 't1', $1, 'h1', 2, 'exited', 2, 1.5), ('p3', 't1', $1, 'h1', 3, 'exited', NULL, NULL)`, r1)
	var run Run
	if code := getJSON(t, s, key, "/v1/runs/"+r1, &run); code != http.StatusOK {
		t.Fatalf("get run: %d", code)
	}
	if len(run.Placements) != 3 || run.Placements[0].PeakCPUCores == nil || *run.Placements[0].PeakCPUCores != 3.25 ||
		run.Placements[2].PeakCPUCores != nil || run.Placements[2].CPUStallSeconds != nil {
		t.Fatalf("placements %+v", run.Placements)
	}
	u := run.Usage
	if u.PeakCPUCores == nil || *u.PeakCPUCores != 3.25 || u.CPUStallSeconds == nil || *u.CPUStallSeconds != 7.5 ||
		u.IOStallSeconds == nil || *u.IOStallSeconds != 4 {
		t.Fatalf("run usage peak %v cpu stall %v io stall %v, want 3.25, 7.5, 4", val(u.PeakCPUCores), val(u.CPUStallSeconds), val(u.IOStallSeconds))
	}
}

// A placement's raw samples carry its CPU rate per interval and the stall
// counters; a rollup keeps the bucket's highest CPU rate and the counters'
// maximum, and the Run's history serves the stalls as rates.
func TestRollupPeakCPUAndStalls(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	// Two samples in each of two minutes: CPU rates 1, 6 | 2, 3; cpu
	// stall 0, 30 | 60, 120 seconds.
	for i, off := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
		execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds, peak_cpu_cores, cpu_stall, io_stall)
			VALUES ($1, 1, 't1', 0, $2, $3, $4, $5, NULL)`, r1, hour.Add(off), float64(i*10),
			[]float64{1, 6, 2, 3}[i], []float64{0, 30, 60, 120}[i])
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	q := "from=" + url.QueryEscape(hour.Add(-time.Second).Format(time.RFC3339)) + "&to=" + url.QueryEscape(hour.Add(time.Hour).Format(time.RFC3339))
	for _, c := range []struct {
		res  string
		want []string
	}{
		// Minutes: the peaks 6 and 3; the stall counter 30 then 120, a
		// rate of 90 s over 60 s.
		{"60", []string{"peak=6 stall=absent io=absent", "peak=3 stall=1.5 io=absent"}},
		{"3600", []string{"peak=6 stall=absent io=absent"}},
	} {
		h := historyRequest(t, s, key, "/v1/runs/"+r1+"/history?res="+c.res+"&"+q)
		var got []string
		for _, sm := range h.Samples {
			got = append(got, fmt.Sprintf("peak=%v stall=%v io=%v", val(sm.PeakCPUCores), val(sm.CPUStallRate), val(sm.IOStallRate)))
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("res %s: %v, want %v", c.res, got, c.want)
		}
	}
	if v := queryOne[float64](t, s, `SELECT cpu_stall FROM placement_samples WHERE res = 3600`); v != 120 {
		t.Errorf("hour cpu_stall %v, want 120", v)
	}
}

// A heartbeat writes the rate over its interval and the stall counters into
// the raw sample; absent values are NULL.
func TestHeartbeatSamplesCPURateAndStalls(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	u := &proto.Usage{CPUSeconds: 3, CPUCores: f64(0.75), PeakCPUCores: f64(2), CPUStallSeconds: f64(1.25), MemoryFullStallSeconds: f64(0.5)}
	if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: u}}}); err != nil {
		t.Fatal(err)
	}
	row := queryOne[[]*float64](t, s, `SELECT ARRAY[peak_cpu_cores, cpu_stall, mem_stall, mem_full_stall, io_stall, io_full_stall]
		FROM placement_samples WHERE run_id = $1 AND res = 0`, r1)
	var got []any
	for _, v := range row {
		got = append(got, val(v))
	}
	if want := []any{0.75, 1.25, "absent", 0.5, "absent", "absent"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sample %v, want %v", got, want)
	}
}

func val(v *float64) any {
	if v == nil {
		return "absent"
	}
	return *v
}
