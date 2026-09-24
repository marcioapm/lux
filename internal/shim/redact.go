package shim

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Redactor replaces secret values in output before it is written anywhere.
// It matches each value exactly and in its common encodings (base64, URL
// encoding, hex). Every occurrence of every form is found and overlapping
// or adjacent matches are merged into one marker, so no part of a value
// survives because another match started first.
type Redactor struct {
	mu      sync.RWMutex
	pats    []pattern // longest first
	longest int
}

type pattern struct{ from, to string }

// minSecretLen: shorter values would redact ordinary text; they are not
// redacted (and documented as such).
const minSecretLen = 4

func NewRedactor(values map[string]string) *Redactor {
	r := &Redactor{}
	r.Set(values)
	return r
}

func (r *Redactor) Set(values map[string]string) {
	seen := map[string]bool{}
	var pairs []pattern
	add := func(v, name string) {
		if len(v) < minSecretLen || seen[v] {
			return
		}
		seen[v] = true
		pairs = append(pairs, pattern{v, "[REDACTED:" + name + "]"})
	}
	for name, v := range values {
		add(v, name)
		add(base64.StdEncoding.EncodeToString([]byte(v)), name)
		add(strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(v)), "="), name)
		add(base64.URLEncoding.EncodeToString([]byte(v)), name)
		add(url.QueryEscape(v), name)
		add(url.PathEscape(v), name)
		add(hex.EncodeToString([]byte(v)), name)
		// JSON-escaped forms, for values with quotes or backslashes that
		// appear inside structured events.
		esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`).Replace(v)
		add(esc, name)
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	longest := 0
	if len(pairs) > 0 {
		longest = len(pairs[0].from) // sorted longest first
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pats, r.longest = pairs, longest
}

func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	type match struct{ start, end, pat int }
	var ms []match
	for i, p := range r.pats {
		// Step one byte past each match start: a value may overlap itself.
		for off := 0; off < len(s); {
			j := strings.Index(s[off:], p.from)
			if j < 0 {
				break
			}
			ms = append(ms, match{off + j, off + j + len(p.from), i})
			off += j + 1
		}
	}
	if len(ms) == 0 {
		return s
	}
	// By start, then longest (pats is longest first): the first match of a
	// merged range names its marker.
	sort.Slice(ms, func(a, b int) bool {
		if ms[a].start != ms[b].start {
			return ms[a].start < ms[b].start
		}
		return ms[a].pat < ms[b].pat
	})
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for k := 0; k < len(ms); {
		start, end, pat := ms[k].start, ms[k].end, ms[k].pat
		for k++; k < len(ms) && ms[k].start <= end; k++ {
			end = max(end, ms[k].end)
		}
		b.WriteString(s[last:start])
		b.WriteString(r.pats[pat].to)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// Longest is the longest matched form, so a streaming writer can hold back
// that many bytes to catch a value split across writes.
func (r *Redactor) Longest() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.longest
}
