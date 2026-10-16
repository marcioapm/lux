package runner

import (
	"slices"
	"testing"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

func cpus(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// Whole-CPU Runs get disjoint sets while the host has free CPUs.
func TestCpusetWholeRunsAreDisjoint(t *testing.T) {
	host := cpus(8)
	var held [][]int
	for _, c := range []float64{2, 2, 3} {
		held = append(held, cpusetFor(host, held, c))
	}
	want := [][]int{{0, 1}, {2, 3}, {4, 5, 6}}
	if !slices.EqualFunc(held, want, slices.Equal) {
		t.Fatalf("sets %v, want %v", held, want)
	}
}

// A fractional Run gets one CPU: the one the fewest Runs hold.
func TestCpusetFractionalSharesTheLeastShared(t *testing.T) {
	host := cpus(4)
	held := [][]int{{0, 1}, {2}, {0}, {3}, {3}}
	if got := cpusetFor(host, held, 0.5); !slices.Equal(got, []int{1}) && !slices.Equal(got, []int{2}) {
		t.Fatalf("0.5 cpus: %v, want one of the CPUs held once (1 or 2)", got)
	}
	if got := cpusetFor(host, held, 0.5); !slices.Equal(got, []int{1}) {
		t.Fatalf("ties go to the lowest CPU: %v", got)
	}
	if got := cpusetFor(host, held, 1.5); len(got) != 2 {
		t.Fatalf("1.5 cpus: %v, want 2 CPUs", got)
	}
}

// An oversubscribed host shares its least-loaded CPUs and never fails; a
// Run asking more CPUs than the host has gets them all.
func TestCpusetOversubscriptionShares(t *testing.T) {
	host := cpus(4)
	held := [][]int{{0, 1, 2}, {3}, {0}}
	if got := cpusetFor(host, held, 2); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("oversubscribed: %v, want [1 2] (each held once)", got)
	}
	if got := cpusetFor(host, held, 16); !slices.Equal(got, host) {
		t.Fatalf("more than the host: %v", got)
	}
	if got := cpusetFor(nil, held, 2); got != nil {
		t.Fatalf("no CPUs: %v", got)
	}
	if got := cpusetFor(host, nil, 0); got != nil {
		t.Fatalf("no cpus asked: %v", got)
	}
}

// What re-adoption does: the sets read back from run state are the held
// sets, so the next Run gets what it would have got before the restart.
func TestCpusetRebuiltFromStateMatches(t *testing.T) {
	host := cpus(6)
	var held []string
	var sets [][]int
	for _, c := range []float64{2, 1, 0.5} {
		s := cpusetFor(host, sets, c)
		sets = append(sets, s)
		held = append(held, cpuList(s)) // as persisted
	}
	var rebuilt [][]int
	for _, s := range held {
		rebuilt = append(rebuilt, parseCPUList(s))
	}
	if !slices.EqualFunc(rebuilt, sets, slices.Equal) {
		t.Fatalf("rebuilt %v, want %v", rebuilt, sets)
	}
	if a, b := cpusetFor(host, sets, 2), cpusetFor(host, rebuilt, 2); !slices.Equal(a, b) || !slices.Equal(a, []int{4, 5}) {
		t.Fatalf("next Run: %v before restart, %v after", a, b)
	}
}

// A released set is free again.
func TestCpusetReleaseFrees(t *testing.T) {
	host := cpus(4)
	a := cpusetFor(host, nil, 2)
	b := cpusetFor(host, [][]int{a}, 2)
	if got := cpusetFor(host, [][]int{a, b}, 2); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("full host shares from the start: %v", got)
	}
	if got := cpusetFor(host, [][]int{b}, 2); !slices.Equal(got, a) {
		t.Fatalf("after releasing %v: %v", a, got)
	}
}

func TestCPUList(t *testing.T) {
	for s, want := range map[string][]int{"0-3,6": {0, 1, 2, 3, 6}, "5": {5}, "1,3,5": {1, 3, 5}, "0-1,1-2": {0, 1, 2}} {
		got := parseCPUList(s)
		if !slices.Equal(got, want) {
			t.Errorf("parse %q: %v", s, got)
		}
		if s != "0-1,1-2" && cpuList(got) != s {
			t.Errorf("format %v: %q", got, cpuList(got))
		}
	}
	for _, bad := range []string{"", "a", "3-1", "-1", "1,"} {
		if got := parseCPUList(bad); got != nil {
			t.Errorf("parse %q: %v", bad, got)
		}
	}
}

// With --cpuset, each new placement is pinned beside the host's live ones,
// those re-adopted from run state included, and its container is created
// with --cpuset-cpus and LUX_CPUSET; without it nothing is pinned.
func TestPlacementsArePinnedBesideLiveOnes(t *testing.T) {
	r := &Runner{hostCPUs: cpus(4), placements: map[string]*placement{}}
	// As readopt leaves a running one: its state as saved.
	r.placements["old"] = &placement{r: r, runID: "old", phase: "running", state: &runState{Cpuset: "0-1"}}
	r.placements["gone"] = &placement{r: r, runID: "gone", phase: "done", state: &runState{Cpuset: "2-3"}}
	p := &placement{r: r, runID: "new", phase: "starting", state: &runState{}}
	r.placements["new"] = p
	r.allocateCpuset(p, 2)
	if got := p.cpuset(); got != "2-3" {
		t.Fatalf("pinned to %q, want 2-3 (0-1 is held; a done placement holds nothing)", got)
	}
	sp := spec.RunSpec{Resources: spec.Resources{CPUs: 2, Memory: 1 << 30, Pids: 64}}
	args := p.createArgs(sp, "img", podman.Network{})
	if i := slices.Index(args, "--cpuset-cpus"); i < 0 || args[i+1] != "2-3" {
		t.Fatalf("create args %v", args)
	}
	if env := withCpusetEnv(map[string]string{}, p.cpuset(), nil); env["LUX_CPUSET"] != "2-3" {
		t.Fatalf("env %v", env)
	}
	if env := withCpusetEnv(map[string]string{"LUX_CPUSET": "x"}, p.cpuset(), map[string]string{"LUX_CPUSET": "x"}); env["LUX_CPUSET"] != "x" {
		t.Fatalf("the spec's own LUX_CPUSET lost: %v", env)
	}

	off := &Runner{placements: map[string]*placement{}}
	q := &placement{r: off, runID: "q", state: &runState{}}
	off.allocateCpuset(q, 2)
	if args := q.createArgs(sp, "img", podman.Network{}); slices.Contains(args, "--cpuset-cpus") || q.cpuset() != "" {
		t.Fatalf("without --cpuset: %v", args)
	}
	if env := withCpusetEnv(map[string]string{}, "", nil); len(env) != 0 {
		t.Fatalf("without --cpuset: env %v", env)
	}
}
