package sizing

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

func some[T any](v T) Optional[T] { return Optional[T]{true, v} }
func shape(cpu float64) Shape {
	return Shape{Dimensions: Dimensions{cpu, spec.Bytes(cpu) * 4 << 30, spec.Bytes(cpu) * 20 << 30}, Family: "m", Arch: "arm64", MinCPUs: 0.5, DefaultCPUs: 2}
}
func model(t *testing.T, cpu float64) *Model {
	t.Helper()
	m, err := New(shape(cpu))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLadders(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		entries := m.List()
		count := 4
		if cpu == 8 {
			count = 5
		}
		if len(entries) != count {
			t.Fatal(entries)
		}
		for i, e := range entries {
			wantCPU := math.Ldexp(0.5, i)
			if e.CPUs != wantCPU || e.Memory != spec.Bytes(wantCPU*4*(1<<30)) || e.Disk != spec.Bytes(wantCPU*20*(1<<30)) || e.Fraction != wantCPU/cpu {
				t.Fatal(e)
			}
			parsed, err := m.Parse(e.Name)
			if err != nil || parsed != e {
				t.Fatalf("parse: %+v %v", parsed, err)
			}
			resolved, err := m.Resolve(Request{Size: some(e.Name)})
			if err != nil || resolved.Resolved != e {
				t.Fatalf("explicit: %+v %v", resolved, err)
			}
		}
		if entries[0].Name != "m-05-arm64" {
			t.Fatal(entries[0])
		}
		entries[0].Name = "changed"
		if m.List()[0].Name != "m-05-arm64" {
			t.Fatal("List leaked storage")
		}
	}
}

func TestShapeValidation(t *testing.T) {
	cases := map[string]func(*Shape){
		"zero CPU": func(s *Shape) { s.CPUs = 0 }, "negative CPU": func(s *Shape) { s.CPUs = -8 },
		"NaN": func(s *Shape) { s.CPUs = math.NaN() }, "Inf": func(s *Shape) { s.CPUs = math.Inf(1) },
		"overflow": func(s *Shape) { s.CPUs = math.MaxFloat64 }, "precision bound": func(s *Shape) { s.CPUs = 1 << 54 },
		"non power": func(s *Shape) { s.CPUs = 6 }, "next CPU": func(s *Shape) { s.CPUs = math.Nextafter(8, 9) },
		"small min": func(s *Shape) { s.MinCPUs = 0.25 }, "non power min": func(s *Shape) { s.MinCPUs = 0.75 },
		"big min": func(s *Shape) { s.MinCPUs = 16 }, "NaN min": func(s *Shape) { s.MinCPUs = math.NaN() },
		"zero min": func(s *Shape) { s.MinCPUs = 0 }, "Inf min": func(s *Shape) { s.MinCPUs = math.Inf(1) },
		"zero memory": func(s *Shape) { s.Memory = 0 }, "negative disk": func(s *Shape) { s.Disk = -1 },
		"memory remainder": func(s *Shape) { s.Memory++ }, "disk remainder": func(s *Shape) { s.Disk += 8 },
		"too few bytes": func(s *Shape) { s.Memory = 8 }, "max int remainder": func(s *Shape) { s.Disk = math.MaxInt64 },
		"default absent": func(s *Shape) { s.DefaultCPUs = 3 }, "default zero": func(s *Shape) { s.DefaultCPUs = 0 },
		"default NaN": func(s *Shape) { s.DefaultCPUs = math.NaN() }, "bad arch": func(s *Shape) { s.Arch = "x86_64" },
		"empty family": func(s *Shape) { s.Family = "" }, "bad family": func(s *Shape) { s.Family = "M!" },
		"long name": func(s *Shape) { s.Family = strings.Repeat("m", MaxName) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := shape(8)
			change(&s)
			if _, err := New(s); err == nil {
				t.Fatal("accepted invalid shape", s)
			}
		})
	}
	s := shape(8)
	s.Memory = math.MaxInt64 - 15
	s.Disk = math.MaxInt64 - 15
	m, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.List() {
		if e.Memory <= 0 || e.Memory > s.Memory || e.Memory != s.Memory/spec.Bytes(1/e.Fraction) {
			t.Fatal(e)
		}
	}
	s = shape(8)
	s.Family = "custom-compute"
	s.Arch = "amd64"
	s.MinCPUs = 1
	s.DefaultCPUs = 4
	m, err = New(s)
	if err != nil {
		t.Fatal(err)
	}
	if m.List()[0].Name != "custom-compute-1-amd64" {
		t.Fatal(m.List())
	}
	s = shape(0.5)
	s.Memory = 2 << 30
	s.Disk = 10 << 30
	s.DefaultCPUs = 0.5
	if _, err = New(s); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalNames(t *testing.T) {
	m := model(t, 8)
	for _, name := range []string{"", "m4", "lux-m-2-arm64", "m-0.5-arm64", "m-005-arm64", "m-02-arm64", "m-2.0-arm64", "m-2e0-arm64", "m-+2-arm64", "m-3-arm64", "m-16-arm64", "m-2-amd64", "c-2-arm64", "M-2-arm64", " m-2-arm64", "m-2-arm64\n", "m--2-arm64"} {
		if _, err := m.Parse(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestResolution(t *testing.T) {
	m := model(t, 8)
	cases := []struct {
		name string
		r    Request
		cpu  float64
	}{
		{"default", Request{}, 2}, {"cpu only", Request{CPUs: some(1.0)}, 1},
		{"3 CPUs 10Gi", Request{CPUs: some(3.0), Memory: some(spec.Bytes(10 << 30))}, 4},
		{"memory determines", Request{CPUs: some(1.0), Memory: some(spec.Bytes(12 << 30))}, 4},
		{"disk determines", Request{Disk: some(spec.Bytes(41 << 30))}, 4},
		{"minimum", Request{CPUs: some(math.SmallestNonzeroFloat64), Memory: some(spec.Bytes(1)), Disk: some(spec.Bytes(1))}, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Resolve(c.r)
			if err != nil || got.Resolved.CPUs != c.cpu || got.Requested != c.r {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	s := shape(8)
	s.DefaultCPUs = 4
	m, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Resolve(Request{})
	if err != nil || got.Resolved.CPUs != 4 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestBoundaries(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		entries := m.List()
		for i, e := range entries {
			exact := []Request{{CPUs: some(e.CPUs)}, {Memory: some(e.Memory)}, {Disk: some(e.Disk)}, {CPUs: some(e.CPUs), Memory: some(e.Memory), Disk: some(e.Disk)}}
			for _, r := range exact {
				got, err := m.Resolve(r)
				if err != nil || got.Resolved != e {
					t.Fatalf("exact %+v: %+v %v", r, got, err)
				}
			}
			next := []Request{{CPUs: some(math.Nextafter(e.CPUs, math.Inf(1)))}, {Memory: some(e.Memory + 1)}, {Disk: some(e.Disk + 1)}}
			for _, r := range next {
				got, err := m.Resolve(r)
				if i == len(entries)-1 {
					var big *OversizedError
					if !errors.As(err, &big) || big.Requested != r || big.Maximum != e || big.Error() == "" {
						t.Fatalf("oversized: %+v %v", got, err)
					}
				} else if err != nil || got.Resolved != entries[i+1] {
					t.Fatalf("next %+v: %+v %v", r, got, err)
				}
			}
		}
	}
}

func TestInvalidRequests(t *testing.T) {
	m := model(t, 8)
	for _, v := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := m.Resolve(Request{CPUs: some(v)}); err == nil {
			t.Fatal(v)
		}
	}
	for _, v := range []spec.Bytes{0, -1, math.MinInt64} {
		for _, r := range []Request{{Memory: some(v)}, {Disk: some(v)}} {
			if _, err := m.Resolve(r); err == nil {
				t.Fatal(r)
			}
		}
	}
	for _, r := range []Request{{CPUs: some(math.MaxFloat64)}, {Memory: some(spec.Bytes(math.MaxInt64))}, {Disk: some(spec.Bytes(math.MaxInt64))}} {
		var big *OversizedError
		_, err := m.Resolve(r)
		if !errors.As(err, &big) {
			t.Fatal(err)
		}
	}
	for _, r := range []Request{{Size: some("")}, {Size: some("m-2-arm64"), CPUs: some(0.0)}, {Size: some("m-2-arm64"), Memory: some(spec.Bytes(1))}, {Size: some("m-2-arm64"), Disk: some(spec.Bytes(0))}} {
		if _, err := m.Resolve(r); err == nil {
			t.Fatal(r)
		}
	}
	r := Request{CPUs: Optional[float64]{Value: math.NaN()}, Memory: Optional[spec.Bytes]{Value: -1}}
	got, err := m.Resolve(r)
	if err != nil || got.Resolved.CPUs != 2 || !math.IsNaN(got.Requested.CPUs.Value) {
		t.Fatalf("omitted values inspected: %+v %v", got, err)
	}
}

func TestGeneratedCoverAndMonotonicity(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		cpus := []float64{math.SmallestNonzeroFloat64}
		mem := []spec.Bytes{1}
		disk := []spec.Bytes{1}
		entries := m.List()
		for _, e := range entries {
			cpus = append(cpus, e.CPUs, math.Nextafter(e.CPUs, math.Inf(1)))
			mem = append(mem, e.Memory, e.Memory+1)
			disk = append(disk, e.Disk, e.Disk+1)
		}
		for _, c := range cpus {
			for _, mb := range mem {
				for _, db := range disk {
					r := Request{CPUs: some(c), Memory: some(mb), Disk: some(db)}
					want := -1
					for i, e := range entries {
						if e.CPUs >= c && e.Memory >= mb && e.Disk >= db {
							want = i
							break
						}
					}
					got, err := m.Resolve(r)
					if want < 0 {
						var big *OversizedError
						if !errors.As(err, &big) {
							t.Fatalf("expected oversized %+v", r)
						}
						continue
					}
					if err != nil || got.Resolved != entries[want] {
						t.Fatalf("smallest cover %+v: %+v %v", r, got, err)
					}
					for _, higher := range []Request{{CPUs: some(math.Nextafter(c, math.Inf(1))), Memory: r.Memory, Disk: r.Disk}, {CPUs: r.CPUs, Memory: some(mb + 1), Disk: r.Disk}, {CPUs: r.CPUs, Memory: r.Memory, Disk: some(db + 1)}} {
						next, err := m.Resolve(higher)
						if err == nil && next.Resolved.CPUs < got.Resolved.CPUs {
							t.Fatal("non-monotonic", r, higher)
						}
					}
				}
			}
		}
	}
}

func TestFallback(t *testing.T) {
	m := model(t, 8)
	before := m.List()
	for _, d := range []Dimensions{{8, 32 << 30, 160 << 30}, {16, 64 << 30, 320 << 30}, {9, 33 << 30, 161 << 30}} {
		if err := m.ValidateFallback("arm64", d); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		arch string
		d    Dimensions
	}{
		{"amd64", shape(8).Dimensions}, {"", shape(8).Dimensions},
		{"arm64", Dimensions{math.Nextafter(8, 0), 32 << 30, 160 << 30}},
		{"arm64", Dimensions{16, (32 << 30) - 1, 320 << 30}}, {"arm64", Dimensions{16, 64 << 30, (160 << 30) - 1}},
		{"arm64", Dimensions{math.NaN(), 64 << 30, 320 << 30}}, {"arm64", Dimensions{math.Inf(1), 64 << 30, 320 << 30}},
	}
	for _, c := range cases {
		if err := m.ValidateFallback(c.arch, c.d); err == nil {
			t.Fatal("invalid fallback", c)
		}
	}
	if !reflect.DeepEqual(before, m.List()) {
		t.Fatal("fallback changed nominal ladder")
	}
}
