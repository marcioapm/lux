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
// encoding, hex), longest first so a value containing another is replaced
// whole.
type Redactor struct {
	mu       sync.RWMutex
	replacer *strings.Replacer
	longest  int
}

// minSecretLen: shorter values would redact ordinary text; they are not
// redacted (and documented as such).
const minSecretLen = 4

func NewRedactor(values map[string]string) *Redactor {
	r := &Redactor{}
	r.Set(values)
	return r
}

func (r *Redactor) Set(values map[string]string) {
	type pair struct{ from, to string }
	seen := map[string]bool{}
	var pairs []pair
	add := func(v, name string) {
		if len(v) < minSecretLen || seen[v] {
			return
		}
		seen[v] = true
		pairs = append(pairs, pair{v, "[REDACTED:" + name + "]"})
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
	args := make([]string, 0, 2*len(pairs))
	longest := 0
	for _, p := range pairs {
		args = append(args, p.from, p.to)
		longest = max(longest, len(p.from))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) == 0 {
		r.replacer, r.longest = nil, 0
		return
	}
	r.replacer, r.longest = strings.NewReplacer(args...), longest
}

func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

// Longest is the longest matched form, so a streaming writer can hold back
// that many bytes to catch a value split across writes.
func (r *Redactor) Longest() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.longest
}
