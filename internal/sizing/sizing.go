// Package sizing resolves nominal coupled pool allocations without applying Run defaults.
package sizing

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"

	"github.com/marcioapm/lux/internal/spec"
)

// MaxName bounds the canonical name independently of future API schemas.
const MaxName = 63

var familyRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type Dimensions struct {
	CPUs   float64    `json:"cpus"`
	Memory spec.Bytes `json:"memory"`
	Disk   spec.Bytes `json:"disk"`
}

type Shape struct {
	Dimensions
	Family      string
	Arch        string
	MinCPUs     float64
	DefaultCPUs float64
}

type Entry struct {
	Dimensions
	Name     string  `json:"name"`
	Fraction float64 `json:"fraction"`
}

// Optional retains explicit zero separately from omission, including in result snapshots.
type Optional[T any] struct {
	Present bool `json:"present"`
	Value   T    `json:"value"`
}

type Request struct {
	Size   Optional[string]     `json:"size"`
	CPUs   Optional[float64]    `json:"cpus"`
	Memory Optional[spec.Bytes] `json:"memory"`
	Disk   Optional[spec.Bytes] `json:"disk"`
}

type Resolution struct {
	Requested Request `json:"requested"`
	Resolved  Entry   `json:"resolved"`
}

type OversizedError struct {
	Requested Request
	Maximum   Entry
}

func (e *OversizedError) Error() string {
	return fmt.Sprintf("request exceeds pool maximum %s: requested %+v; maximum cpus=%g memory=%d disk=%d", e.Maximum.Name, e.Requested, e.Maximum.CPUs, e.Maximum.Memory, e.Maximum.Disk)
}

// Model owns a validated ladder, ascending from the minimum to the whole host.
type Model struct {
	shape        Shape
	entries      []Entry
	defaultEntry Entry
}

func positive(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func powerOfTwo(v float64) bool {
	fraction, _ := math.Frexp(v)
	return positive(v) && fraction == 0.5
}

func New(s Shape) (*Model, error) {
	if !familyRE.MatchString(s.Family) || (s.Arch != "arm64" && s.Arch != "amd64") {
		return nil, fmt.Errorf("invalid family or architecture: %q/%q", s.Family, s.Arch)
	}
	// 0.5 is the smallest nameable CPU tier; integer tokens stay exactly representable.
	if !powerOfTwo(s.CPUs) || !powerOfTwo(s.MinCPUs) || s.MinCPUs < 0.5 || s.CPUs < s.MinCPUs || s.CPUs > 1<<53 {
		return nil, fmt.Errorf("CPU shape must be powers of two with 0.5 <= minimum <= nominal <= 2^53")
	}
	if s.Memory <= 0 || s.Disk <= 0 {
		return nil, fmt.Errorf("nominal memory and disk must be positive bytes")
	}
	m := &Model{shape: s}
	memory, disk := s.Memory, s.Disk
	fraction := 1.0
	for cpu := s.CPUs; ; cpu /= 2 {
		token := strconv.FormatFloat(cpu, 'f', 0, 64)
		if cpu == 0.5 {
			token = "05"
		}
		name := s.Family + "-" + token + "-" + s.Arch
		if len(name) > MaxName {
			return nil, fmt.Errorf("size name exceeds %d characters: %q", MaxName, name)
		}
		e := Entry{Dimensions: Dimensions{cpu, memory, disk}, Name: name, Fraction: fraction}
		m.entries = append(m.entries, e)
		if cpu == s.DefaultCPUs {
			m.defaultEntry = e
		}
		if cpu == s.MinCPUs {
			break
		}
		if memory%2 != 0 || disk%2 != 0 || memory/2 == 0 || disk/2 == 0 {
			return nil, fmt.Errorf("nominal bytes must divide exactly into positive bytes at every tier")
		}
		memory /= 2
		disk /= 2
		fraction /= 2
	}
	if m.defaultEntry.Name == "" {
		return nil, fmt.Errorf("default CPUs %g is not a ladder entry", s.DefaultCPUs)
	}
	slices.Reverse(m.entries)
	return m, nil
}

func (m *Model) List() []Entry { return slices.Clone(m.entries) }

// Parse accepts only canonical names belonging to this nominal pool ladder.
func (m *Model) Parse(name string) (Entry, error) {
	for _, e := range m.entries {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("size %q is not in family %q architecture %q ladder", name, m.shape.Family, m.shape.Arch)
}

func covers(e Entry, r Request) bool {
	return (!r.CPUs.Present || e.CPUs >= r.CPUs.Value) &&
		(!r.Memory.Present || e.Memory >= r.Memory.Value) &&
		(!r.Disk.Present || e.Disk >= r.Disk.Value)
}

func (m *Model) Resolve(r Request) (Resolution, error) {
	raw := r.CPUs.Present || r.Memory.Present || r.Disk.Present
	if r.Size.Present && raw {
		return Resolution{}, fmt.Errorf("explicit size conflicts with raw CPU/memory/disk fields")
	}
	if r.Size.Present {
		e, err := m.Parse(r.Size.Value)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{r, e}, nil
	}
	if (r.CPUs.Present && !positive(r.CPUs.Value)) || (r.Memory.Present && r.Memory.Value <= 0) || (r.Disk.Present && r.Disk.Value <= 0) {
		return Resolution{}, fmt.Errorf("supplied CPU/memory/disk fields must be finite and positive")
	}
	if !raw {
		return Resolution{r, m.defaultEntry}, nil
	}
	for _, e := range m.entries {
		if covers(e, r) {
			return Resolution{r, e}, nil
		}
	}
	return Resolution{}, &OversizedError{r, m.entries[len(m.entries)-1]}
}

// ValidateFallback checks nominal capabilities, not live free capacity or runner protocol support.
func (m *Model) ValidateFallback(arch string, d Dimensions) error {
	if arch != m.shape.Arch {
		return fmt.Errorf("fallback architecture %q differs from shape %q", arch, m.shape.Arch)
	}
	if !positive(d.CPUs) || d.CPUs < m.shape.CPUs || d.Memory < m.shape.Memory || d.Disk < m.shape.Disk {
		return fmt.Errorf("fallback capacities %+v do not cover nominal shape %+v", d, m.shape.Dimensions)
	}
	return nil
}
