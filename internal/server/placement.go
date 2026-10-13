package server

import (
	"cmp"
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

// allocated is the share of h's tightest resource already reserved: the
// largest of used/capacity over the cpus, memory and disk it advertises
// (a resource it does not limit counts 0). Run counts are not resources.
func (h *candidateHost) allocated() float64 {
	share := func(used, capacity float64) float64 {
		if capacity <= 0 {
			return 0
		}
		return used / capacity
	}
	return max(share(h.UsedCPUs, h.Capacity.CPUs),
		share(float64(h.UsedMem), float64(h.Capacity.Memory)),
		share(float64(h.UsedDisk), float64(h.Capacity.Disk)))
}

// packCandidate is a host a Run fits, as pack orders it.
type packCandidate struct {
	h        *candidateHost
	avoid    bool    // the Run must avoid it: last, whatever its allocation
	used     float64 // allocated()
	affinity int     // snapshot copy, cached image and mirrors, preferred labels
}

// packOrder puts the host to choose first: hosts the Run need not avoid,
// then the most allocated, then the highest affinity, then the lowest id.
func packOrder(a, b packCandidate) int {
	if a.avoid != b.avoid {
		if a.avoid {
			return 1
		}
		return -1
	}
	if c := cmp.Compare(b.used, a.used); c != 0 {
		return c
	}
	if c := cmp.Compare(b.affinity, a.affinity); c != 0 {
		return c
	}
	return cmp.Compare(a.h.ID, b.h.ID)
}
