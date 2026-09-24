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
	if got := r.Redact("abc"); got != "abc" || r.Longest() != 0 {
		t.Errorf("got %q, longest %d", got, r.Longest())
	}
}
