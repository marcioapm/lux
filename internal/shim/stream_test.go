package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/vterm"
)

// ptyTerminal is a terminal on a fresh PTY pair: what is written to the
// returned tty is what a workload writes. Its output is read as the
// output file reads it, into read (when not nil).
func ptyTerminal(t *testing.T, rows, cols, history int, read func([]byte)) (*terminal, *os.File, chan struct{}) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	if err := pty.Setsize(ptmx, winsize(rows, cols)); err != nil {
		t.Fatal(err)
	}
	term := newTerminal(ptmx, rows, cols)
	term.screen = vterm.New(rows, cols, history)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			if n > 0 && read != nil {
				read(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		tty.Close()
		ptmx.Close()
		<-done
	})
	return term, tty, done
}

// screenOf returns the terminal's emulator state as a snapshot, under its
// lock.
func screenOf(term *terminal) []byte {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.screen.Snapshot(0)
}

// waitScreen waits until the emulator shows want: all written before it
// has been read.
func waitScreen(t *testing.T, term *terminal, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(screenOf(term), []byte(want)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the terminal never showed %q", want)
}

// replay is a fresh emulator fed a snapshot and the output after it.
func replay(rows, cols, history int, chunks ...[]byte) *vterm.Terminal {
	v := vterm.New(rows, cols, history)
	for _, c := range chunks {
		v.Write(c)
	}
	return v
}

func screenText(v *vterm.Terminal) string {
	rows, cols := v.Size()
	var b strings.Builder
	for y := range rows {
		var l strings.Builder
		for x := range cols {
			l.WriteString(v.Text(v.Cell(x, y)))
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestAttachRestoresPlainScreen(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 6, 40, historyLines, nil)
	fmt.Fprint(tty, "\x1b]2;my title\x07$ echo hi\r\nhi\r\n$ ")
	waitScreen(t, term, "$ echo hi")

	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(6, 40, historyLines, snap)
	if got := screenText(v); got != "$ echo hi\nhi\n$\n\n\n\n" {
		t.Fatalf("restored screen %q", got)
	}
	if x, y, _ := v.Cursor(); x != 2 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	if v.Title() != "my title" {
		t.Fatalf("title %q", v.Title())
	}
	// Without restore, no snapshot: output from now on, as always.
	if snap, sub := term.join(false, 0, 0); snap != nil {
		t.Fatalf("a snapshot without restore: %q", snap)
	} else {
		term.leave(sub)
	}
}

func TestAttachRestoreScrollbackBounded(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 5, 20, 100, nil)
	for i := range 1000 {
		fmt.Fprintf(tty, "line %d\r\n", i)
	}
	fmt.Fprint(tty, "end")
	waitScreen(t, term, "end")
	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(5, 20, 100000, snap)
	if n := v.HistoryLen(); n != 100 {
		t.Fatalf("history %d lines, want the limit, 100", n)
	}
	first := v.HistoryLine(0)
	var s strings.Builder
	for _, c := range first {
		s.WriteString(v.Text(c))
	}
	// 1000 lines then "end": 996–999 and "end" on screen, 100 before.
	if s.String() != "line 896" {
		t.Fatalf("oldest history line %q", s.String())
	}
}

func TestAttachRestoresAltScreen(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 8, 40, historyLines, nil)
	fmt.Fprint(tty, "$ make\r\nbuilt ok\r\n$ top\r\n")
	fmt.Fprint(tty, "\x1b[?1049h\x1b[?1h\x1b=\x1b[?1000h\x1b[?1006h\x1b[2J\x1b[H\x1b[7m PID  CPU \x1b[m\x1b[3;1H  42  99%")
	waitScreen(t, term, "  42  99%")

	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(8, 40, historyLines, snap)
	if !v.AltScreen() || !v.Mode(1049) || !v.Mode(1) || !v.Mode(1000) || !v.Mode(1006) {
		t.Fatalf("alt screen %v, modes 1049 %v 1 %v 1000 %v 1006 %v", v.AltScreen(), v.Mode(1049), v.Mode(1), v.Mode(1000), v.Mode(1006))
	}
	if got := screenText(v); !strings.HasPrefix(got, " PID  CPU\n\n  42  99%\n") {
		t.Fatalf("alt screen %q", got)
	}
	if c := v.Cell(1, 0); c.Style.Attr&vterm.AttrReverse == 0 {
		t.Fatalf("header not reverse: %+v", c)
	}

	// The program exits the full screen: the client, fed only the live
	// output after its snapshot, is back on the main screen.
	fmt.Fprint(tty, "\x1b[?1000l\x1b[?1006l\x1b[?1l\x1b>\x1b[?1049l$ ")
	var live []byte
	deadline := time.After(10 * time.Second)
	for !bytes.Contains(live, []byte("1049l$ ")) {
		select {
		case b := <-sub:
			live = append(live, b...)
		case <-deadline:
			t.Fatalf("live output %q", live)
		}
	}
	v.Write(live)
	if v.AltScreen() || v.Mode(1000) || v.Mode(1) {
		t.Fatal("still on the alternate screen, or its modes")
	}
	if got := screenText(v); !strings.HasPrefix(got, "$ make\nbuilt ok\n$ top\n$\n") {
		t.Fatalf("main screen after leaving %q", got)
	}
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("the client's terminal and the shim's differ")
	}
}

func TestAttachRestoreResizesFirst(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	fmt.Fprint(tty, "hello")
	waitScreen(t, term, "hello")
	snap, sub := term.join(true, 10, 30)
	defer term.leave(sub)
	ws, err := pty.GetsizeFull(term.ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Rows != 10 || ws.Cols != 30 {
		t.Fatalf("pty %dx%d, want 10x30", ws.Rows, ws.Cols)
	}
	v := replay(10, 30, historyLines, snap)
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("snapshot not of the new size")
	}
	term.resize(12, 50)
	term.mu.Lock()
	r, c := term.screen.(*vterm.Terminal).Size()
	term.mu.Unlock()
	if r != 12 || c != 50 {
		t.Fatalf("emulator %dx%d after a resize, want 12x50", r, c)
	}
}

// Attaches during a fast writer: each one's snapshot and the live output
// after it, replayed, are exactly the shim's terminal at the end — no
// output lost or repeated between them.
func TestAttachRestoreNoGapNoDuplicate(t *testing.T) {
	const rows, cols, history = 8, 40, 1 << 20
	term, tty, _ := ptyTerminal(t, rows, cols, history, nil)

	type attached struct {
		snap []byte
		live bytes.Buffer
		done chan struct{}
	}
	var mu sync.Mutex
	var all []*attached
	stop := make(chan struct{})
	attach := func() {
		a := &attached{done: make(chan struct{})}
		snap, sub := term.join(true, 0, 0)
		a.snap = snap
		go func() {
			defer close(a.done)
			for {
				select {
				case b := <-sub:
					a.live.Write(b)
				case <-stop:
					// Drain what was sent before the stop.
					for {
						select {
						case b := <-sub:
							a.live.Write(b)
						default:
							term.leave(sub)
							return
						}
					}
				}
			}
		}()
		mu.Lock()
		all = append(all, a)
		mu.Unlock()
	}

	// The writer goes on until every attach has happened, so all of them
	// land while output is flowing.
	const attaches = 40
	attachesDone := make(chan struct{})
	writerDone := make(chan struct{})
	written := 0
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-attachesDone:
				fmt.Fprint(tty, "THE END")
				return
			default:
			}
			// Colours and cursor movement too, so a byte out of place shows.
			fmt.Fprintf(tty, "\x1b[3%dm%05d\x1b[m %s\r\n", i%8, i, strings.Repeat("x", i%30))
			if i%500 == 0 {
				fmt.Fprint(tty, "\x1b[2;3H\x1b[K*\x1b[8;1H")
			}
			written = i
		}
	}()
	for i := range attaches {
		attach()
		time.Sleep(time.Duration(i%3) * time.Millisecond)
	}
	close(attachesDone)
	<-writerDone
	waitScreen(t, term, "THE END")
	close(stop)
	final := screenOf(term)
	for i, a := range all {
		<-a.done
		v := replay(rows, cols, history, a.snap, a.live.Bytes())
		if got := v.Snapshot(0); !bytes.Equal(got, final) {
			t.Fatalf("attach %d of %d (snapshot %d bytes, live %d): replayed terminal differs from the shim's", i, len(all), len(a.snap), a.live.Len())
		}
	}
	if written < 100 {
		t.Fatalf("only %d lines written during the attaches", written)
	}
}

// A subscriber that never reads holds up neither the workload's output
// nor the reader that feeds the output file and the emulator.
func TestSlowSubscriberNeverBlocks(t *testing.T) {
	var n int64
	var nmu sync.Mutex
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, func(b []byte) {
		nmu.Lock()
		n += int64(len(b))
		nmu.Unlock()
	})
	_, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	line := strings.Repeat("y", 79) + "\n"
	total := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20000 { // ~1.6 MB: thousands of reads past the subscriber's buffer
			io.WriteString(tty, line)
			total += len(line) + 1 // ONLCR: \n goes out as \r\n
		}
		io.WriteString(tty, "DONE")
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the workload's writes blocked")
	}
	waitScreen(t, term, "DONE")
	nmu.Lock()
	defer nmu.Unlock()
	if n != int64(total+4) {
		t.Fatalf("output read %d bytes, want %d", n, total+4)
	}
}

type failingScreen struct{ calls int }

func (f *failingScreen) Write(p []byte) {
	f.calls++
	if f.calls > 1 {
		panic("emulator bug")
	}
}
func (f *failingScreen) Resize(rows, cols int) {}
func (f *failingScreen) Snapshot(int) []byte   { return []byte("snap") }

// An emulator that fails is dropped: output and attach go on, without
// restore.
func TestEmulatorFailureKeepsStream(t *testing.T) {
	var got bytes.Buffer
	var gmu sync.Mutex
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, func(b []byte) {
		gmu.Lock()
		got.Write(b)
		gmu.Unlock()
	})
	term.mu.Lock()
	term.screen = &failingScreen{}
	term.mu.Unlock()
	_, sub := term.join(false, 0, 0)
	defer term.leave(sub)

	io.WriteString(tty, "one")
	waitOutput := func(want string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			gmu.Lock()
			ok := strings.Contains(got.String(), want)
			gmu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("output never had %q", want)
	}
	waitOutput("one")
	io.WriteString(tty, "two") // the emulator panics on this one
	waitOutput("two")
	io.WriteString(tty, "three")
	waitOutput("three")

	var live []byte
	deadline := time.After(10 * time.Second)
	for !bytes.Contains(live, []byte("three")) {
		select {
		case b := <-sub:
			live = append(live, b...)
		case <-deadline:
			t.Fatalf("subscriber got %q", live)
		}
	}
	if string(live) != "onetwothree" {
		t.Fatalf("subscriber got %q", live)
	}
	snap, sub2 := term.join(true, 0, 0)
	term.leave(sub2)
	if snap != nil {
		t.Fatalf("a snapshot from a dropped emulator: %q", snap)
	}
	term.resize(10, 10) // no emulator: just the PTY
}

// The shim's attach stream sends the snapshot as stdout StreamData,
// chunked, then live output.
func TestAttachStreamSendsSnapshotFrames(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 30, 200, historyLines, nil)
	// A screen whose snapshot is over one chunk.
	for i := range 29 {
		fmt.Fprintf(tty, "\x1b[3%dm%s\x1b[m\r\n", i%8, strings.Repeat(fmt.Sprint(i%10), 199))
	}
	fmt.Fprint(tty, "ready")
	waitScreen(t, term, "ready")
	s := &Shim{term: term}

	runnerSide, shimSide := net.Pipe()
	defer runnerSide.Close()
	go func() {
		defer shimSide.Close()
		sc := bufio.NewScanner(shimSide)
		s.handleStream(sc, json.NewEncoder(shimSide), proto.StreamOpen{Kind: "attach", Restore: true})
	}()
	dec := json.NewDecoder(runnerSide)
	var frames []proto.StreamData
	var out []byte
	sent := false
	for !bytes.Contains(out, []byte("after")) {
		var d proto.StreamData
		runnerSide.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("after %d frames: %v", len(frames), err)
		}
		if d.Channel != "stdout" || d.Error != "" {
			t.Fatalf("frame %+v", d)
		}
		if len(d.Data) > streamChunk {
			t.Fatalf("a frame of %d bytes", len(d.Data))
		}
		frames = append(frames, d)
		out = append(out, d.Data...)
		if !sent && bytes.Contains(out, []byte("ready")) {
			sent = true
			fmt.Fprint(tty, " after")
		}
	}
	if len(frames) < 2 {
		t.Fatalf("%d frames", len(frames))
	}
	v := replay(30, 200, historyLines, out)
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("the stream does not reproduce the terminal")
	}
}
