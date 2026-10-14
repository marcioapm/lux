package server

import (
	"cmp"
	"math"
	"net/http"
)

// A pool's placement policy (pools.placement): how pickHost orders the
// hosts a Run fits.
const (
	PlacementSpread = "spread"
	PlacementPack   = "pack"
)

// ValidPlacement refuses a placement other than spread, pack or "" (the
// pool's default) with 422 invalid_pool.
func ValidPlacement(p string) error {
	switch p {
	case "", PlacementSpread, PlacementPack:
		return nil
	}
	return errf(http.StatusUnprocessableEntity, "invalid_pool", "placement %q: want spread or pack", p)
}

// packs reports whether a pool with stored placement p ("" for none)
// packs: pack when set so, else when the pool is shaped.
func packs(p string, shaped bool) bool {
	return p == PlacementPack || p == "" && shaped
}

// allocated is the share of h's tightest resource reserved by normal work:
// the largest of used/capacity over the cpus, memory and disk it advertises
// (a resource it does not limit counts 0). Run counts are not resources.
// Spot placements (h.Spots) count as free, as in the planner's readyHosts:
// a host holding only spot work is idle to scale-down, so pack must not
// rank it full and keep it alive with normal Runs. hostFit still counts them.
func (h *candidateHost) allocated() float64 {
	cpus, mem, disk := h.UsedCPUs, h.UsedMem, h.UsedDisk
	for _, sp := range h.Spots {
		cpus -= sp.Res.CPUs
		mem -= int64(sp.Res.Memory)
		disk -= int64(sp.Res.Disk)
	}
	share := func(used, capacity float64) float64 {
		if capacity <= 0 {
			return 0
		}
		return used / capacity
	}
	return max(share(cpus, h.Capacity.CPUs),
		share(float64(mem), float64(h.Capacity.Memory)),
		share(float64(disk), float64(h.Capacity.Disk)))
}

// packCandidate is a host a Run fits, as pack orders it.
type packCandidate struct {
	h        *candidateHost
	avoid    bool    // the Run must avoid it: last, whatever its allocation
	used     float64 // allocated()
	affinity int     // snapshot copy, cached image and mirrors, preferred labels
}

// packCandidateOf is h as pack orders it for a Run avoiding avoidHost
// ("": none, so a planned host without an id is never avoided).
func packCandidateOf(h *candidateHost, avoidHost string, affinity int) packCandidate {
	return packCandidate{h: h, avoid: avoidHost != "" && h.ID == avoidHost, used: h.allocated(), affinity: affinity}
}

// packOrder puts the host to choose first: hosts the Run need not avoid,
// then the most allocated, then the highest affinity, then the lowest id.
// Allocations compare as math.Round(used*1e9), one key per host so the
// order stays total; float sums such as 0.1+0.2 and 0.3 tie and affinity
// decides.
func packOrder(a, b packCandidate) int {
	if a.avoid != b.avoid {
		if a.avoid {
			return 1
		}
		return -1
	}
	if c := cmp.Compare(math.Round(b.used*1e9), math.Round(a.used*1e9)); c != 0 {
		return c
	}
	if c := cmp.Compare(b.affinity, a.affinity); c != 0 {
		return c
	}
	return cmp.Compare(a.h.ID, b.h.ID)
}
