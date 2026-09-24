package shim

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	// byFirst indexes pats by first byte, for held.
	byFirst [256][]int
	// multi are the forms that contain a newline: only they span lines.
	multi []int
}

type pattern struct{ from, to string }

// maxCutSteps bounds Cut's walk back through overlapping matches.
const maxCutSteps = 8

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
		b64 := base64.StdEncoding.EncodeToString([]byte(v))
		add(v, name)
		add(b64, name)
		add(strings.TrimRight(b64, "="), name)
		add(base64.URLEncoding.EncodeToString([]byte(v)), name)
		add(url.QueryEscape(v), name)
		add(url.PathEscape(v), name)
		add(hex.EncodeToString([]byte(v)), name)
		// JSON-escaped forms, as they appear inside events: as json.Marshal
		// writes them (& < > become \u0026 …), and without HTML escaping.
		add(jsonString(v, true), name)
		add(jsonString(v, false), name)
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	longest := 0
	if len(pairs) > 0 {
		longest = len(pairs[0].from) // sorted longest first
	}
	var byFirst [256][]int
	var multi []int
	for i, p := range pairs {
		byFirst[p.from[0]] = append(byFirst[p.from[0]], i)
		if strings.Contains(p.from, "\n") {
			multi = append(multi, i)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pats, r.longest, r.byFirst, r.multi = pairs, longest, byFirst, multi
}

func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sps := r.spans(s)
	if len(sps) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, sp := range sps {
		b.WriteString(s[last:sp.start])
		b.WriteString(r.pats[sp.pat].to)
		last = sp.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// span is a redacted range: overlapping or adjacent matches merged, named
// by the first (then longest) of them.
type span struct{ start, end, pat int }

// spans must be called with mu held.
func (r *Redactor) spans(s string) []span {
	var ms []span
	for i, p := range r.pats {
		// Step one byte past each match start: a value may overlap itself.
		for off := 0; off < len(s); {
			j := strings.Index(s[off:], p.from)
			if j < 0 {
				break
			}
			ms = append(ms, span{off + j, off + j + len(p.from), i})
			off += j + 1
		}
	}
	if len(ms) == 0 {
		return nil
	}
	// By start, then longest (pats is longest first): the first match of a
	// merged range names its marker.
	sort.Slice(ms, func(a, b int) bool {
		if ms[a].start != ms[b].start {
			return ms[a].start < ms[b].start
		}
		return ms[a].pat < ms[b].pat
	})
	out := ms[:0]
	for k := 0; k < len(ms); {
		sp := ms[k]
		for k++; k < len(ms) && ms[k].start <= sp.end; k++ {
			sp.end = max(sp.end, ms[k].end)
		}
		out = append(out, sp)
	}
	return out
}

// Cut returns the largest i <= end at which a stream buffer b can be split
// and b[:i] redacted on its own without leaking part of a value: not
// inside a match, and not inside a tail of b that may be the start of a
// value whose rest is not written yet.
func (r *Redactor) Cut(b []byte, end int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	end = min(end, len(b))
	if r.longest == 0 || end <= 0 {
		return end
	}
	// Just after a newline, only a value spanning lines can cross the cut
	// or be held: none, for most runs, so line output costs nothing here.
	if b[end-1] == '\n' && len(r.multi) == 0 {
		return end
	}
	end = min(end, len(b)-r.held(b))
	// Back to the start of any match crossing end; that may land inside
	// another. A long chain of overlapping matches (a buffer covered by
	// values) has no safe cut worth finding: 0, and the writer releases
	// it whole when it must.
	for steps := 0; end > 0 && end < len(b); steps++ {
		if steps == maxCutSteps {
			return 0
		}
		c := end
		wlo := max(0, end-r.longest+1)
		w := string(b[wlo:min(len(b), end+r.longest-1)])
		try := func(k int) {
			from := r.pats[k].from
			lo, hi := max(0, end-len(from)+1)-wlo, min(len(b), end+len(from)-1)-wlo
			// Any match in w[lo:hi] crosses end.
			if j := strings.Index(w[lo:hi], from); j >= 0 {
				c = min(c, wlo+lo+j)
			}
		}
		if b[end-1] == '\n' {
			for _, k := range r.multi {
				try(k)
			}
		} else {
			for k := range r.pats {
				try(k)
			}
		}
		if c == end {
			break
		}
		end = c
	}
	return end
}

// held is the length of the longest suffix of b that is a proper prefix of
// some form: bytes that may start a value still being written. Before the
// tail's last newline, only forms spanning lines are tried. Must be called
// with mu held.
func (r *Redactor) held(b []byte) int {
	lo := max(0, len(b)-r.longest+1)
	nl := lo + bytes.LastIndexByte(b[lo:], '\n') // lo-1 if none
	n := 0
	for _, k := range r.multi {
		from := r.pats[k].from
		for i := max(lo, len(b)-len(from)+1); i <= nl && len(b)-i > n; i++ {
			if b[i] == from[0] && from[:len(b)-i] == string(b[i:]) {
				n = len(b) - i
			}
		}
	}
	if n > 0 {
		return n
	}
	for i := max(lo, nl+1); i < len(b); i++ {
		t := b[i:]
		for _, k := range r.byFirst[t[0]] {
			if from := r.pats[k].from; len(from) > len(t) && from[:len(t)] == string(t) {
				return len(t)
			}
		}
	}
	return 0
}

// jsonString is v as a JSON string literal, without the quotes.
func jsonString(v string, escapeHTML bool) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(escapeHTML)
	_ = enc.Encode(v)
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(b.String(), "\n"), `"`), `"`)
}
