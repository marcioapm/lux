package shim

import "testing"

// Overlapping, adjacent and nested values are redacted whole, as one
// marker named by the earliest (then longest) match.
func TestRedactOverlaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets map[string]string
		in, out string
	}{
		{"overlap", map[string]string{"A": "abcdefgh", "B": "wxyzabcd"},
			"x wxyzabcdefgh y", "x [REDACTED:B] y"},
		{"overlap reversed", map[string]string{"A": "abcdefgh", "B": "efghijkl"},
			"abcdefghijkl", "[REDACTED:A]"},
		{"adjacent", map[string]string{"A": "aaaa1", "B": "bbbb2"},
			"-aaaa1bbbb2-", "-[REDACTED:A]-"},
		{"nested", map[string]string{"OUTER": "0123456789", "INNER": "3456"},
			"<0123456789>", "<[REDACTED:OUTER]>"},
		{"nested inner alone", map[string]string{"OUTER": "0123456789", "INNER": "3456"},
			"x3456x", "x[REDACTED:INNER]x"},
		{"same start, longest names", map[string]string{"LONG": "abcdefgh", "SHORT": "abcd"},
			"abcdefgh", "[REDACTED:LONG]"},
		{"self overlap", map[string]string{"A": "abab"},
			"ababab", "[REDACTED:A]"},
		{"separate", map[string]string{"A": "abcdefgh"},
			"abcdefgh abcdefgh", "[REDACTED:A] [REDACTED:A]"},
		{"none", map[string]string{"A": "abcdefgh"}, "nothing here", "nothing here"},
	} {
		if got := NewRedactor(tc.secrets).Redact(tc.in); got != tc.out {
			t.Errorf("%s: Redact(%q) = %q, want %q", tc.name, tc.in, got, tc.out)
		}
	}
}

func TestRedactEmpty(t *testing.T) {
	r := NewRedactor(nil)
	if got := r.Redact("abc"); got != "abc" || r.longest != 0 {
		t.Errorf("got %q, longest %d", got, r.longest)
	}
}

// Values with & < > are matched as json.Marshal writes them (& …).
func TestRedactJSONEscaped(t *testing.T) {
	r := NewRedactor(map[string]string{"K": `a&b<c>"d`})
	for _, in := range []string{`a&b<c>\"d`, `a&b<c>\"d`, `a&b<c>"d`} {
		if got := r.Redact("x " + in + " y"); got != "x [REDACTED:K] y" {
			t.Errorf("Redact(%q) = %q", in, got)
		}
	}
}

// Cut never lands inside a value, nor before a tail that may start one.
func TestRedactCut(t *testing.T) {
	r := NewRedactor(map[string]string{"K": "supersecretvalue", "L": "abcd", "M": "cdefgh", "N": "efghijkl"})
	for _, tc := range []struct {
		in       string
		end, cut int
	}{
		{"xx supersecretvalue yy", 8, 3},   // inside a value: back to its start
		{"xx supersecretvalue yy", 22, 22}, // clear of it
		{"xx supersecretvalue yy", 3, 3},   // at its start
		{"xx supersecretvalue yy", 19, 19}, // at its end
		{"xxabcdsupersecretvalue", 10, 6},  // between adjacent values is safe
		{"xxabcdefghijkl", 12, 2},          // overlapping: back to the first
		{"hello\ntoken=supers", 18, 12},    // a tail that may start a value
		{"hello\ntoken=supers", 6, 6},      // an earlier cut is not affected
		{"plain text", 10, 10},
	} {
		if got := r.Cut([]byte(tc.in), tc.end); got != tc.cut {
			t.Errorf("Cut(%q, %d) = %d, want %d", tc.in, tc.end, got, tc.cut)
		}
	}
	if got := NewRedactor(nil).Cut([]byte("abc"), 2); got != 2 {
		t.Errorf("no values: Cut = %d", got)
	}
}
