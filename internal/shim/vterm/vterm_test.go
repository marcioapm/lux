package vterm

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
)

// screenText is the current screen as text, one line per row, trailing
// blanks trimmed.
func screenText(t *Terminal) string {
	var b strings.Builder
	for y := range t.rows {
		var l strings.Builder
		for x := range t.cols {
			l.WriteString(t.Text(t.Cell(x, y)))
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func historyText(t *Terminal) []string {
	var out []string
	for i := range t.HistoryLen() {
		var l strings.Builder
		for _, c := range t.HistoryLine(i) {
			l.WriteString(t.Text(c))
		}
		out = append(out, strings.TrimRight(l.String(), " "))
	}
	return out
}

func feed(t *Terminal, s string) { t.Write([]byte(s)) }

// restored is a fresh terminal of t's size that has been sent t's
// snapshot.
func restored(t *Terminal, history int) *Terminal {
	r := New(t.rows, t.cols, history)
	r.Write(t.Snapshot())
	return r
}

// sameState fails unless a and b show the same cells (text and style),
// cursor, modes, title and scroll region, on both screens, and the same
// history.
func sameState(tb testing.TB, a, b *Terminal) {
	tb.Helper()
	if a.rows != b.rows || a.cols != b.cols {
		tb.Fatalf("size %dx%d, want %dx%d", b.rows, b.cols, a.rows, a.cols)
	}
	cmpScreen := func(name string, sa, sb *screen) {
		for y := range a.rows {
			for x := range a.cols {
				ca, cb := sa.lines[y][x], sb.lines[y][x]
				if a.Text(ca) != b.Text(cb) || ca.Style != cb.Style || ca.Flags != cb.Flags {
					tb.Fatalf("%s cell %d,%d: got %q %+v flags %d, want %q %+v flags %d", name, x, y,
						b.Text(cb), cb.Style, cb.Flags, a.Text(ca), ca.Style, ca.Flags)
				}
			}
		}
	}
	cmpScreen("main", &a.main, &b.main)
	if a.AltScreen() != b.AltScreen() {
		tb.Fatalf("alt screen %v, want %v", b.AltScreen(), a.AltScreen())
	}
	if a.AltScreen() {
		cmpScreen("alt", &a.alt, &b.alt)
	}
	ax, ay, av := a.Cursor()
	bx, by, bv := b.Cursor()
	if ax != bx || ay != by || av != bv || a.cur.wrapNext != b.cur.wrapNext {
		tb.Fatalf("cursor %d,%d visible %v wrap %v, want %d,%d visible %v wrap %v", bx, by, bv, b.cur.wrapNext, ax, ay, av, a.cur.wrapNext)
	}
	if a.cur.pen != b.cur.pen {
		tb.Fatalf("pen %+v, want %+v", b.cur.pen, a.cur.pen)
	}
	for _, n := range append([]int{6, 7, 25, 47, 1047, 1049}, replayModes...) {
		if a.Mode(n) != b.Mode(n) {
			tb.Fatalf("mode %d: %v, want %v", n, b.Mode(n), a.Mode(n))
		}
	}
	if a.keypad != b.keypad || a.insert != b.insert || a.newline != b.newline || a.cursorStyle != b.cursorStyle {
		tb.Fatalf("keypad/insert/newline/cursor style differ")
	}
	if a.Title() != b.Title() {
		tb.Fatalf("title %q, want %q", b.Title(), a.Title())
	}
	at, ab := a.ScrollRegion()
	bt, bb := b.ScrollRegion()
	if at != bt || ab != bb {
		tb.Fatalf("scroll region %d-%d, want %d-%d", bt, bb, at, ab)
	}
	ha, hb := historyText(a), historyText(b)
	if strings.Join(ha, "\n") != strings.Join(hb, "\n") {
		tb.Fatalf("history:\n%q\nwant:\n%q", hb, ha)
	}
}

func TestPlainScreen(t *testing.T) {
	term := New(5, 20, 100)
	feed(term, "hello\r\nworld\r\n\tx")
	if got, want := screenText(term), "hello\nworld\n        x\n\n\n"; got != want {
		t.Fatalf("screen %q, want %q", got, want)
	}
	if x, y, _ := term.Cursor(); x != 9 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	r := restored(term, 100)
	if got := screenText(r); got != screenText(term) {
		t.Fatalf("restored screen %q", got)
	}
	sameState(t, term, r)
}

func TestScrollbackBounded(t *testing.T) {
	term := New(4, 10, 50)
	for i := range 1000 {
		fmt.Fprintf(writer{term}, "line %d\r\n", i)
	}
	if n := term.HistoryLen(); n != 50 {
		t.Fatalf("history %d lines, want 50", n)
	}
	h := historyText(term)
	// 1000 lines and a blank one being written: the screen holds 997–999
	// and the blank, the history the 50 before.
	if h[0] != "line 947" || h[49] != "line 996" {
		t.Fatalf("history from %q to %q", h[0], h[49])
	}
	r := restored(term, 50)
	sameState(t, term, r)
}

// A long run of output keeps memory bounded by the history limit: the
// ring's slots are reused, not grown.
func TestScrollbackMemoryBounded(t *testing.T) {
	term := New(24, 80, 2000)
	line := bytes.Repeat([]byte("x"), 79)
	line = append(line, "\r\n"...)
	for range 10000 {
		term.Write(line)
	}
	if len(term.hist.lines) != 2000 {
		t.Fatalf("history slots %d", len(term.hist.lines))
	}
	for i := range term.hist.lines {
		if c := cap(term.hist.lines[i]); c > 80 {
			t.Fatalf("history line %d holds %d cells", i, c)
		}
	}
}

type writer struct{ t *Terminal }

func (w writer) Write(p []byte) (int, error) { w.t.Write(p); return len(p), nil }

func TestAltScreen(t *testing.T) {
	for _, mode := range []int{1049, 1047, 47} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			term := New(6, 30, 100)
			feed(term, "\x1b]2;my app\x07")
			feed(term, "shell$ run\r\nmain screen text\r\n")
			fmt.Fprintf(writer{term}, "\x1b[?%dh", mode)
			feed(term, "\x1b[2J\x1b[H\x1b[1;37;44m full screen \x1b[m\x1b[3;5Hmenu\x1b[?1h\x1b=\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[2;5r\x1b[4;2H")
			if !term.AltScreen() {
				t.Fatal("not on the alternate screen")
			}
			r := restored(term, 100)
			sameState(t, term, r)
			if !r.AltScreen() || !r.Mode(mode) || !r.Mode(1002) || !r.Mode(1006) || !r.Mode(2004) || !r.Mode(1) {
				t.Fatalf("modes not restored: alt %v", r.AltScreen())
			}
			if got := screenText(r); !strings.Contains(got, " full screen") || !strings.Contains(got, "menu") {
				t.Fatalf("alt screen %q", got)
			}
			if r.Title() != "my app" {
				t.Fatalf("title %q", r.Title())
			}

			// The program leaves the full screen: both get the same
			// bytes and must end up with the same main screen.
			leave := fmt.Sprintf("\x1b[r\x1b[?%dl\x1b[?1l\x1b>back\r\n", mode)
			feed(term, leave)
			feed(r, leave)
			sameState(t, term, r)
			if got := screenText(r); !strings.Contains(got, "main screen text") || strings.Contains(got, "menu") {
				t.Fatalf("main screen after leaving: %q", got)
			}
		})
	}
}

func TestAttributesColoursWideChars(t *testing.T) {
	term := New(6, 20, 100)
	feed(term, "\x1b[1;3;4mbiu\x1b[m \x1b[38;5;200;48;2;1;2;3mx\x1b[m \x1b[91;102mbr\x1b[m\r\n")
	feed(term, "\x1b[4:3mcurl\x1b[24m \x1b[7;9;2mrs\x1b[m\r\n")
	feed(term, "宽字符ok\r\n")
	feed(term, "e\u0301 a\u0308\r\n")
	feed(term, "\x1b[38:2::10:20:30mc\x1b[m")
	// A wide character that does not fit wraps.
	feed(term, "\x1b[5;20H宽")

	c := term.Cell(0, 0)
	if term.Text(c) != "b" || c.Style.Attr&(AttrBold|AttrItalic) != AttrBold|AttrItalic || c.Style.Attr.Underline() != 1 {
		t.Fatalf("cell 0,0 %+v", c)
	}
	if c := term.Cell(4, 0); c.Style.Fg != Indexed(200) || c.Style.Bg != RGB(1, 2, 3) {
		t.Fatalf("cell 4,0 %+v", c)
	}
	if c := term.Cell(6, 0); c.Style.Fg != Indexed(9) || c.Style.Bg != Indexed(10) {
		t.Fatalf("cell 6,0 %+v", c)
	}
	if c := term.Cell(0, 1); c.Style.Attr.Underline() != 3 {
		t.Fatalf("curly underline %+v", c)
	}
	if c := term.Cell(0, 2); term.Text(c) != "宽" || c.Flags != FlagWide || term.Cell(1, 2).Flags != FlagCont || term.Text(term.Cell(6, 2)) != "o" {
		t.Fatalf("wide chars: %q", screenText(term))
	}
	if term.Text(term.Cell(0, 3)) != "e\u0301" || term.Text(term.Cell(2, 3)) != "a\u0308" {
		t.Fatalf("combining: %q %q", term.Text(term.Cell(0, 3)), term.Text(term.Cell(2, 3)))
	}
	if c := term.Cell(0, 4); c.Style.Fg != RGB(10, 20, 30) {
		t.Fatalf("colon rgb %+v", c)
	}
	if term.Text(term.Cell(0, 5)) != "宽" {
		t.Fatalf("wrapped wide char: %q", screenText(term))
	}
	sameState(t, term, restored(term, 100))
}

func TestPendingWrapAndModes(t *testing.T) {
	term := New(3, 5, 10)
	feed(term, "abcde") // the cursor waits at the last column
	feed(term, "\x1b[?25l\x1b[5 q\x1b[4h\x1b(0")
	r := restored(term, 10)
	sameState(t, term, r)
	feed(term, "fq")
	feed(r, "fq")
	sameState(t, term, r)

	term = New(4, 10, 10)
	feed(term, "\x1b[?7l0123456789XY\x1b[2;3r\x1b[?6h\x1b[2;4H")
	r = restored(term, 10)
	sameState(t, term, r)
	feed(term, "z\nw")
	feed(r, "z\nw")
	sameState(t, term, r)
}

func TestSavedCursorRestored(t *testing.T) {
	term := New(5, 20, 10)
	feed(term, "\x1b[3;7H\x1b[31m\x1b7\x1b[m\x1b[1;1Htop")
	r := restored(term, 10)
	sameState(t, term, r)
	feed(term, "\x1b8x")
	feed(r, "\x1b8x")
	sameState(t, term, r)
}

func TestResize(t *testing.T) {
	term := New(5, 10, 100)
	for i := range 5 {
		fmt.Fprintf(writer{term}, "row %d\r\n", i)
	}
	feed(term, "last")
	term.Resize(3, 6)
	if r, c := term.Size(); r != 3 || c != 6 {
		t.Fatalf("size %dx%d", r, c)
	}
	if got := screenText(term); got != "row 3\nrow 4\nlast\n" {
		t.Fatalf("after shrinking %q", got)
	}
	if x, y, _ := term.Cursor(); x != 4 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	sameState(t, term, restored(term, 100))
	term.Resize(8, 30)
	sameState(t, term, restored(term, 100))
	term.Resize(100000, 100000)
	if r, c := term.Size(); r != MaxRows || c != MaxCols {
		t.Fatalf("not clamped: %dx%d", r, c)
	}
}

// Random output, including escape sequences cut anywhere, never panics,
// and its snapshot always reproduces it.
func TestRandomOutputRoundTrips(t *testing.T) {
	pieces := []string{
		"a", "xy", "宽", "e\u0301", "\r", "\n", "\t", "\b", "\x1b[", "31m", "\x1b[m", "\x1b[1;4;7m",
		"\x1b[2J", "\x1b[K", "\x1b[1K", "\x1b[3;4H", "\x1b[5A", "\x1b[2B", "\x1b[3C", "\x1b[D",
		"\x1b[2L", "\x1b[M", "\x1b[2@", "\x1b[P", "\x1b[X", "\x1b[S", "\x1b[T", "\x1b[2;5r", "\x1b[r",
		"\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?47l", "\x1b[?1h", "\x1b[?2004h", "\x1b[?7l", "\x1b[?7h",
		"\x1b[?6h", "\x1b[?6l", "\x1b[4h", "\x1b[4l", "\x1b7", "\x1b8", "\x1bM", "\x1bD", "\x1bE",
		"\x1b]0;title\x07", "\x1b]2;t2\x1b\\", "\x1bP1$r\x1b\\", "\x1b(0", "\x1b(B", "lqqk", "\x0e", "\x0f",
		"\x1b[38;5;123m", "\x1b[48;2;9;8;7m", "\x1b[?25l", "\x1b[?25h", "\x1bc", "\x1b[3J", "\x1b[!p",
		"\xff", "\xe5\xae", "\x1b[3g", "\x1bH", "\x1b[Z", "\x1b[20h", "\x1b[20l",
	}
	// Not here: REP (CSI b), which repeats the last character printed;
	// a snapshot does not carry that character.
	seed := uint64(1)
	if s, err := strconv.ParseUint(os.Getenv("VTERM_SEED"), 10, 64); err == nil {
		seed = s
	}
	rng := rand.New(rand.NewPCG(seed, 2))
	for iter := range 300 {
		rows, cols := 2+rng.IntN(10), 2+rng.IntN(30)
		term := New(rows, cols, 20)
		var all []byte
		for range 200 {
			all = append(all, pieces[rng.IntN(len(pieces))]...)
		}
		// Fed in random cuts: sequences split across writes, and the
		// snapshot taken at a random cut, maybe mid-sequence. The output
		// after it, fed to both, leaves them the same.
		cut := rng.IntN(len(all) + 1)
		var r *Terminal
		for rest := all; len(rest) > 0; {
			n := 1 + rng.IntN(min(len(rest), 17))
			if done := len(all) - len(rest); done < cut && done+n > cut {
				n = cut - done
			}
			term.Write(rest[:n])
			if r != nil {
				r.Write(rest[:n])
			}
			rest = rest[n:]
			if r == nil && len(all)-len(rest) >= cut {
				r = restored(term, 20)
			}
		}
		if r == nil {
			r = restored(term, 20)
		}
		// Both end the same way, so that a sequence left open does not
		// hide a difference.
		term.Write([]byte("\x18\x1b\\ "))
		r.Write([]byte("\x18\x1b\\ "))
		func() {
			defer func() {
				if t.Failed() {
					t.Logf("iteration %d, %dx%d, input %q", iter, rows, cols, all)
				}
			}()
			sameState(t, term, r)
		}()
		if t.Failed() {
			return
		}
	}
}

func BenchmarkWrite(b *testing.B) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"yes", "y\r\n"},
		{"text", "the quick brown fox jumps over the lazy dog \x1b[31mred\x1b[0m and some more text to fill 80 cols\r\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			chunk := bytes.Repeat([]byte(tc.line), 32<<10/len(tc.line))
			term := New(24, 80, 2000)
			b.SetBytes(int64(len(chunk)))
			for b.Loop() {
				term.Write(chunk)
			}
		})
	}
}
