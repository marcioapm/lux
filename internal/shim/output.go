package shim

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/marcioapm/lux/internal/proto"
)

// Output writes a placement's records to its output file: one JSON line
// each, sequenced, redacted. The file is the record: the runner tails it
// to relay live output and uploads it when the placement ends.
//
// stdout and stderr are buffered per channel up to a newline (or 64 KiB,
// or 100 ms of quiet) so a secret split across two writes is still
// redacted whole.
type Output struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	seq    int64
	red    *Redactor
	bufs   map[string]*chanBuf
	closed bool
}

type chanBuf struct {
	data  []byte
	timer *time.Timer
}

const (
	flushBytes = 64 << 10
	flushAfter = 100 * time.Millisecond
)

// OpenOutput opens (or continues) an output file.
func OpenOutput(path string, red *Redactor) (*Output, error) {
	var seq int64
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var r struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Seq > seq {
				seq = r.Seq
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Output{f: f, w: bufio.NewWriterSize(f, 64<<10), seq: seq, red: red, bufs: map[string]*chanBuf{}}, nil
}

func (o *Output) Seq() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
}

// Write buffers bytes for a stream channel (stdout | stderr).
func (o *Output) Write(ch string, p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	b := o.bufs[ch]
	if b == nil {
		b = &chanBuf{}
		o.bufs[ch] = b
	}
	b.data = append(b.data, p...)
	// Flush complete lines now, keeping a partial last line.
	if i := lastNewline(b.data); i >= 0 {
		o.record(ch, b.data[:i+1], nil)
		b.data = append([]byte{}, b.data[i+1:]...)
	}
	if len(b.data) >= flushBytes {
		cut := len(b.data) - o.red.Longest()
		if cut > 0 {
			cut = utf8Boundary(b.data, cut)
			o.record(ch, b.data[:cut], nil)
			b.data = append([]byte{}, b.data[cut:]...)
		}
	}
	if len(b.data) > 0 {
		if b.timer == nil {
			b.timer = time.AfterFunc(flushAfter, func() { o.flushChan(ch) })
		} else {
			b.timer.Reset(flushAfter)
		}
	}
	o.w.Flush()
}

func (o *Output) flushChan(ch string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	if b := o.bufs[ch]; b != nil && len(b.data) > 0 {
		o.record(ch, b.data, nil)
		b.data = nil
	}
	o.w.Flush()
}

// Event writes a structured event record.
func (o *Output) Event(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{}`)
	}
	ev, _ := json.Marshal(map[string]json.RawMessage{"type": mustJSON(typ), "data": raw})
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	// Flush pending stream bytes first so records stay in order.
	for ch, b := range o.bufs {
		if len(b.data) > 0 {
			o.record(ch, b.data, nil)
			b.data = nil
		}
	}
	red := o.red.Redact(string(ev))
	o.record("event", nil, json.RawMessage(red))
	o.w.Flush()
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// record must be called with mu held.
func (o *Output) record(ch string, data []byte, event json.RawMessage) {
	o.seq++
	r := proto.Record{Seq: o.seq, Time: time.Now().UnixMilli(), Ch: ch}
	if event != nil {
		if !json.Valid(event) {
			event = mustJSON(string(event))
		}
		r.Event = event
	} else {
		r.Data = o.red.Redact(strings.ToValidUTF8(string(data), "�"))
	}
	b, _ := json.Marshal(r)
	o.w.Write(b)
	o.w.WriteByte('\n')
}

// Close flushes everything and closes the file.
func (o *Output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	for ch, b := range o.bufs {
		if b.timer != nil {
			b.timer.Stop()
		}
		if len(b.data) > 0 {
			o.record(ch, b.data, nil)
			b.data = nil
		}
	}
	o.closed = true
	o.w.Flush()
	o.f.Sync()
	return o.f.Close()
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

func utf8Boundary(b []byte, i int) int {
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}
