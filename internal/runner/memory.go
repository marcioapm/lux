package runner

import (
	"fmt"
	"math/bits"
	"os"
)

// DefaultMemoryHeadroom is the memory left to the kernel and the host's own
// processes when --memory-headroom is not given.
const DefaultMemoryHeadroom = 512 << 20

// share is allocatable/capacity, never above 1: what part of a nominal
// amount a host really has. The zero value scales nothing.
type share struct {
	allocatable, capacity uint64
}

// newShare is allocatable over capacity; a capacity of 0 or one not above
// allocatable is not scaled. allocatable must be positive.
func newShare(allocatable, capacity int64) share {
	if capacity <= 0 || allocatable >= capacity {
		return share{allocatable: 1, capacity: 1}
	}
	return share{allocatable: uint64(allocatable), capacity: uint64(capacity)}
}

func (s share) factor() float64 {
	if s.capacity == 0 {
		return 1
	}
	return float64(s.allocatable) / float64(s.capacity)
}

func (s share) scaled() bool { return s.capacity != 0 && s.allocatable != s.capacity }

// of is floor(n × allocatable / capacity) in exact integer arithmetic: never
// above n. n not positive, or nothing scaled, is n.
func (s share) of(n int64) int64 {
	if n <= 0 || !s.scaled() {
		return n
	}
	hi, lo := bits.Mul64(uint64(n), s.allocatable)
	q, _ := bits.Div64(hi, lo, s.capacity) // allocatable < capacity: q <= n, no overflow
	return int64(q)
}

// memoryScale turns what a Run asks for, in the terms of the memory the host
// offers (Config.Memory, the machine's gross size), into its container's
// limit: the same share of what Linux can give Runs, MemTotal - headroom.
// A host offering no more than that is not scaled, nor is anything by the
// zero value.
type memoryScale struct{ share }

func newMemoryScale(memTotal, headroom, capacity int64) (memoryScale, error) {
	if memTotal <= 0 {
		return memoryScale{}, fmt.Errorf("cannot read MemTotal from /proc/meminfo")
	}
	if headroom < 0 {
		return memoryScale{}, fmt.Errorf("--memory-headroom must not be negative")
	}
	allocatable := memTotal - headroom
	if allocatable <= 0 {
		return memoryScale{}, fmt.Errorf("--memory-headroom %d leaves no memory for Runs (MemTotal %d)", headroom, memTotal)
	}
	return memoryScale{newShare(allocatable, capacity)}, nil
}

// limit is share.of(requested) down to whole pages when scaled: cgroup v2
// keeps memory.max in pages, so this is the limit the container really
// has. At least 1 byte: 0 is "no limit" to podman.
func (m memoryScale) limit(requested int64) int64 {
	if requested <= 0 || !m.scaled() {
		return requested
	}
	page := uint64(os.Getpagesize())
	return int64(max(uint64(m.of(requested))/page*page, 1))
}
