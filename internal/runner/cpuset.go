package runner

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// holding is a pinned container's set and the CPUs of time it was given
// (resources.cpus), spread evenly over the set as its demand.
type holding struct {
	set  []int
	cpus float64
}

// demandEpsilon absorbs float rounding in sums of fractional demands
// (0.1 + 0.9 is a full CPU).
const demandEpsilon = 1e-9

// cpusetFor picks ceil(cpus) of the host's CPUs for a Run, given what the
// host's pinned containers hold. Each CPU's demand is the sum, over the
// holdings containing it, of cpus/len(set). A fractional Run (cpus < 1)
// goes to the CPU whose demand is highest yet still fits it within one CPU
// (best fit), so fractions pack together and leave free CPUs to whole-CPU
// Runs; with no such CPU it takes the least-demand one. Any other Run takes
// the least-demand CPUs: entirely free ones first, then the least loaded.
// Ties go to the lowest CPU number. Sets are chosen at placement only and
// never moved after (no re-pinning), so a released set is only reused by
// later placements. Never fails: with no CPUs (or cpus <= 0) it returns
// nil, and the Run is not pinned.
func cpusetFor(host []int, held []holding, cpus float64) []int {
	n := int(math.Ceil(cpus))
	if n <= 0 || len(host) == 0 {
		return nil
	}
	n = min(n, len(host))
	demand := make(map[int]float64, len(host))
	for _, h := range held {
		if len(h.set) == 0 {
			continue
		}
		for _, c := range h.set {
			demand[c] += h.cpus / float64(len(h.set))
		}
	}
	order := slices.Clone(host)
	slices.Sort(order)
	slices.SortStableFunc(order, func(a, b int) int { return cmpDemand(demand[a], demand[b]) })
	if cpus < 1 {
		best := -1
		for _, c := range order {
			d := demand[c]
			if d > demandEpsilon && d+cpus <= 1+demandEpsilon && (best < 0 || d > demand[best]+demandEpsilon) {
				best = c
			}
		}
		if best >= 0 {
			return []int{best}
		}
	}
	out := slices.Clone(order[:n])
	slices.Sort(out)
	return out
}

func cmpDemand(a, b float64) int {
	switch {
	case a < b-demandEpsilon:
		return -1
	case a > b+demandEpsilon:
		return 1
	}
	return 0
}

// cpuList is a set in the kernel's cpu-list form ("0-3,6"), as
// --cpuset-cpus takes it.
func cpuList(set []int) string {
	var parts []string
	for i := 0; i < len(set); {
		j := i
		for j+1 < len(set) && set[j+1] == set[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(set[i]))
		} else {
			parts = append(parts, strconv.Itoa(set[i])+"-"+strconv.Itoa(set[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// parseCPUList reads the kernel's cpu-list form; nil if malformed.
func parseCPUList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// cpuset is the set this placement's container is pinned to; "" if none.
func (p *placement) cpuset() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == nil {
		return ""
	}
	return p.state.Cpuset
}

// allocateCpuset pins p, about to get a container, to its share of the
// host's CPUs, given what the host's other live placements hold (their run
// state, so a re-adopted placement counts as before the restart). Under
// r.mu, so two placements starting at once do not pick from the same view.
// The choice is saved with the container's id (createContainer).
func (r *Runner) allocateCpuset(p *placement, cpus float64) {
	if r.hostCPUs == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var held []holding
	for _, o := range r.placements {
		if o == p || o.liveState() == "" {
			continue
		}
		h := holding{set: parseCPUList(o.cpuset())}
		o.mu.Lock()
		if o.state != nil && o.state.Spec != nil {
			h.cpus = o.state.Spec.Resources.CPUs
		}
		o.mu.Unlock()
		if h.set != nil {
			if h.cpus <= 0 {
				h.cpus = float64(len(h.set))
			}
			held = append(held, h)
		}
	}
	set := cpuList(cpusetFor(r.hostCPUs, held, cpus))
	p.mu.Lock()
	p.state.Cpuset = set
	p.mu.Unlock()
}

// withCpusetEnv adds LUX_CPUSET, the set in cpu-list form, unless the
// spec's env sets it.
func withCpusetEnv(env map[string]string, set string, specEnv map[string]string) map[string]string {
	if _, ok := specEnv["LUX_CPUSET"]; set == "" || ok {
		return env
	}
	env["LUX_CPUSET"] = set
	return env
}
