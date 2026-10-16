package runner

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// cpusetFor picks ceil(cpus) of the host's CPUs for a Run, given the sets
// the host's other Runs hold. It prefers the CPUs held by the fewest Runs,
// lowest number first, so whole-CPU Runs get disjoint sets while capacity
// allows, a fractional Run gets the least-shared CPU, and an oversubscribed
// host shares its least-loaded CPUs. It never fails: with no CPUs (or cpus
// <= 0) it returns nil, and the Run is not pinned.
func cpusetFor(host []int, held [][]int, cpus float64) []int {
	n := int(math.Ceil(cpus))
	if n <= 0 || len(host) == 0 {
		return nil
	}
	n = min(n, len(host))
	load := make(map[int]int, len(host))
	for _, set := range held {
		for _, c := range set {
			load[c]++
		}
	}
	order := slices.Clone(host)
	slices.Sort(order)
	slices.SortStableFunc(order, func(a, b int) int { return load[a] - load[b] })
	out := order[:n]
	slices.Sort(out)
	return out
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
	var held [][]int
	for _, o := range r.placements {
		if o == p || o.liveState() == "" {
			continue
		}
		if set := parseCPUList(o.cpuset()); set != nil {
			held = append(held, set)
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
