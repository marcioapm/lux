package vterm

import (
	"strconv"
)

// Snapshot is a byte sequence that, written to a blank terminal of the
// same size, reproduces this one: it resets the modes Snapshot restores,
// clears the screen, writes the history lines (so they scroll into the
// receiving terminal's own history) and the main screen, then the
// alternate screen if it is shown, and finally the scroll region, modes,
// cursor and title.
//
// It does not send RIS (ESC c): on many terminals that also wipes their
// scrollback, which a person attaching keeps.
func (t *Terminal) Snapshot() []byte {
	b := make([]byte, 0, (t.hist.n+2*t.rows)*(t.cols+16)+256)
	b = append(b, "\x1b[?25l\x1b[?1049l\x1b[r\x1b[m\x1b[?6l\x1b[?7h\x1b[4l\x1b[20l\x1b>\x1b(B\x1b)B\x0f\x1b[0 q"...)
	for _, n := range replayModes {
		b = csiMode(b, n, false)
	}
	b = append(b, "\x1b[H\x1b[2J"...)

	for i := 0; i < t.hist.n; i++ {
		b = t.appendLine(b, t.HistoryLine(i), true)
		b = append(b, "\r\n"...)
	}
	for y, l := range t.main.lines {
		if y > 0 {
			b = append(b, "\r\n"...)
		}
		b = t.appendLine(b, l, true)
	}

	if t.scr == &t.alt {
		ms := t.main.saved
		if t.altMode == 1049 {
			// 1049 saves the main screen's cursor itself, as it was then.
			if ms.valid {
				b = appendCursorState(t, b, ms)
			}
			b = append(b, "\x1b[?1049h"...)
			b = resetCursorState(b)
		} else {
			b = t.appendSaved(b, ms)
			b = csiMode(b, t.altMode, true)
		}
		b = append(b, "\x1b[m\x1b[H\x1b[2J"...)
		for y, l := range t.alt.lines {
			b = cup(b, y, 0)
			b = t.appendLine(b, l, false)
		}
		b = t.appendSaved(b, t.alt.saved)
	} else {
		b = t.appendSaved(b, t.main.saved)
	}

	b = t.appendTabs(b)
	if t.top != 0 || t.bot != t.rows {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(t.top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(t.bot), 10)
		b = append(b, 'r')
	}
	for _, n := range replayModes {
		if t.dec[n] {
			b = csiMode(b, n, true)
		}
	}
	if t.keypad {
		b = append(b, "\x1b="...)
	}
	if !t.autowrap {
		b = csiMode(b, 7, false)
	}

	// The cursor. Pending wrap (the last column written, the next
	// character going to the next line) is reproduced by writing that
	// column's character again.
	b = append(b, "\x1b[m"...)
	if t.origin {
		b = csiMode(b, 6, true)
	}
	y := t.cur.y
	if t.origin {
		y -= t.top
	}
	if t.cur.wrapNext {
		x := t.cols - 1
		l := t.scr.lines[t.cur.y]
		if l[x].Flags&FlagCont != 0 && x > 0 {
			x--
		}
		b = cup(b, y, x)
		b = t.appendSGR(b, l[x].Style)
		b = append(b, t.Text(l[x])...)
	} else {
		b = cup(b, y, t.cur.x)
	}
	if t.insert {
		b = append(b, "\x1b[4h"...)
	}
	if t.newline {
		b = append(b, "\x1b[20h"...)
	}
	if t.g[0] == csGraphics {
		b = append(b, "\x1b(0"...)
	}
	if t.g[1] == csGraphics {
		b = append(b, "\x1b)0"...)
	}
	if t.gl == 1 {
		b = append(b, 0x0e)
	}
	b = t.appendSGR(b, t.cur.pen)
	if t.cursorStyle != 0 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(t.cursorStyle), 10)
		b = append(b, " q"...)
	}
	if t.title != "" {
		b = append(b, "\x1b]2;"...)
		b = append(b, t.title...)
		b = append(b, 0x07)
	}
	if !t.cursorHidden {
		b = append(b, "\x1b[?25h"...)
	}
	return append(b, t.pending()...)
}

// appendTabs sets the tab stops, when they are not the default every 8
// columns. It moves the cursor.
func (t *Terminal) appendTabs(b []byte) []byte {
	def := true
	for x, set := range t.tabs {
		if set != (x > 0 && x%8 == 0) {
			def = false
			break
		}
	}
	if def {
		return b
	}
	b = append(b, "\x1b[3g"...)
	for x, set := range t.tabs {
		if set {
			b = cup(b, 0, x)
			b = append(b, "\x1bH"...)
		}
	}
	return b
}

// appendSaved reproduces a screen's saved cursor (DECSC): its position,
// pen, origin mode and character sets.
func (t *Terminal) appendSaved(b []byte, s savedCursor) []byte {
	if !s.valid {
		return b
	}
	b = appendCursorState(t, b, s)
	b = append(b, "\x1b7"...)
	return resetCursorState(b)
}

// appendCursorState sets what DECSC saves to s. The scroll region is the
// whole screen when it is called, so origin mode leaves positions as
// they are.
func appendCursorState(t *Terminal, b []byte, s savedCursor) []byte {
	if s.origin {
		b = csiMode(b, 6, true)
	}
	if s.g[0] == csGraphics {
		b = append(b, "\x1b(0"...)
	}
	if s.g[1] == csGraphics {
		b = append(b, "\x1b)0"...)
	}
	if s.gl == 1 {
		b = append(b, 0x0e)
	}
	b = cup(b, s.y, s.x)
	return t.appendSGR(b, s.pen)
}

func resetCursorState(b []byte) []byte {
	return append(b, "\x1b[?6l\x1b(B\x1b)B\x0f\x1b[m"...)
}

// appendLine writes a line's cells from the cursor at its column 0. With
// trim, trailing blanks in the default style are left out (the line was
// cleared); the style is reset at the end, so a scroll that follows
// clears in the default background.
func (t *Terminal) appendLine(b []byte, l []Cell, trim bool) []byte {
	end := len(l)
	if trim {
		for end > 0 && l[end-1] == (Cell{}) {
			end--
		}
	}
	var pen Style
	for x := 0; x < end; x++ {
		c := l[x]
		if c.Flags&FlagCont != 0 && x > 0 && l[x-1].Flags&FlagWide != 0 {
			continue
		}
		if c.Style != pen {
			b = t.appendSGR(b, c.Style)
			pen = c.Style
		}
		if c.Flags&FlagCont != 0 {
			b = append(b, ' ')
			continue
		}
		b = append(b, t.Text(c)...)
	}
	if pen != (Style{}) {
		b = append(b, "\x1b[m"...)
	}
	return b
}

func (t *Terminal) appendSGR(b []byte, s Style) []byte {
	b = append(b, "\x1b[0"...)
	a := s.Attr
	for _, f := range []struct {
		bit Attr
		n   byte
	}{{AttrBold, '1'}, {AttrFaint, '2'}, {AttrItalic, '3'}, {AttrBlink, '5'}, {AttrReverse, '7'}, {AttrInvisible, '8'}, {AttrStrike, '9'}} {
		if a&f.bit != 0 {
			b = append(b, ';', f.n)
		}
	}
	switch u := a.Underline(); u {
	case 0:
	case 1:
		b = append(b, ";4"...)
	default:
		b = append(b, ";4:"...)
		b = strconv.AppendInt(b, int64(u), 10)
	}
	b = appendColor(b, s.Fg, 30, 90, "38")
	b = appendColor(b, s.Bg, 40, 100, "48")
	return append(b, 'm')
}

func appendColor(b []byte, c Color, base, bright int, ext string) []byte {
	switch c & colorKind {
	case colorIndexed:
		n := int(c & 0xff)
		switch {
		case n < 8:
			b = append(b, ';')
			b = strconv.AppendInt(b, int64(base+n), 10)
		case n < 16:
			b = append(b, ';')
			b = strconv.AppendInt(b, int64(bright+n-8), 10)
		default:
			b = append(b, ';')
			b = append(b, ext...)
			b = append(b, ";5;"...)
			b = strconv.AppendInt(b, int64(n), 10)
		}
	case colorRGB:
		b = append(b, ';')
		b = append(b, ext...)
		b = append(b, ";2;"...)
		b = strconv.AppendInt(b, int64(c>>16&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c>>8&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c&0xff), 10)
	}
	return b
}

// cup moves the cursor to row y, column x (0-based).
func cup(b []byte, y, x int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)
	return append(b, 'H')
}

func csiMode(b []byte, n int, on bool) []byte {
	b = append(b, "\x1b[?"...)
	b = strconv.AppendInt(b, int64(n), 10)
	if on {
		return append(b, 'h')
	}
	return append(b, 'l')
}
