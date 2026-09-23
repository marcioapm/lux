package shim

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func TestRedactEncodings(t *testing.T) {
	r := NewRedactor(map[string]string{"TOKEN": "s3cr3t-value", "SHORT": "ab"})
	for _, in := range []string{
		"s3cr3t-value",
		base64.StdEncoding.EncodeToString([]byte("s3cr3t-value")),
		"s3cr3t%2Dvalue", // not an encoding we produce; must stay
	} {
		out := r.Redact("x " + in + " y")
		if in == "s3cr3t%2Dvalue" {
			continue
		}
		if strings.Contains(out, in) {
			t.Errorf("%q not redacted: %q", in, out)
		}
	}
	if got := r.Redact("ab"); got != "ab" {
		t.Errorf("short values are not redacted, got %q", got)
	}
}

func readRecords(t *testing.T, path string) []proto.Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []proto.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // as the runner reads them
	for sc.Scan() {
		var r proto.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A secret split across two writes is still redacted whole.
func TestOutputRedactsAcrossWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, err := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	if err != nil {
		t.Fatal(err)
	}
	o.Write("stdout", []byte("token=supers"))
	o.Write("stdout", []byte("ecretvalue done\n"))
	o.Event("x", map[string]string{"v": "supersecretvalue"})
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supersecretvalue") {
		t.Fatalf("secret leaked: %s", b)
	}
	recs := readRecords(t, path)
	if len(recs) != 2 || recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Fatalf("records: %+v", recs)
	}
}

// A partial line is flushed after a short quiet period, and sequence
// numbers continue when the file is reopened (a runner restart).
func TestOutputFlushesAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(nil))
	o.Write("stdout", []byte("no newline"))
	time.Sleep(3 * flushAfter)
	if recs := readRecords(t, path); len(recs) != 1 || recs[0].Data != "no newline" {
		t.Fatalf("not flushed: %+v", recs)
	}
	o.Close()
	o2, _ := OpenOutput(path, NewRedactor(nil))
	o2.Write("stderr", []byte("more\n"))
	o2.Close()
	recs := readRecords(t, path)
	if recs[len(recs)-1].Seq != 2 || recs[1].Ch != "stderr" {
		t.Fatalf("seq did not continue: %+v", recs)
	}
}

// EndLine ends a message streamed in pieces on its own line, and adds
// nothing after text that already ended one.
func TestOutputEndLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(nil))
	o.Write("stdout", []byte("hel"))
	o.Write("stdout", []byte("lo"))
	o.EndLine("stdout")
	o.Write("stdout", []byte("done\n"))
	o.EndLine("stdout")
	o.EndLine("stderr") // nothing written there: nothing to end
	o.Close()
	var text string
	for _, r := range readRecords(t, path) {
		text += r.Data
	}
	if text != "hello\ndone\n" {
		t.Fatalf("got %q", text)
	}
}

// An input ack repeats what was delivered, capped, and redacts secrets in
// it like all output.
func TestInputAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	red := NewRedactor(map[string]string{"TOKEN": "s3cr3t-value"})
	out, err := OpenOutput(path, red)
	if err != nil {
		t.Fatal(err)
	}
	k := &sink{s: &Shim{out: out}}
	k.InputAck(proto.Input{RequestID: "prompt", Text: "use s3cr3t-value please"}, nil)
	k.InputAck(proto.Input{RequestID: "big", Text: strings.Repeat("x", maxAckedText+10)}, nil)
	k.InputAck(proto.Input{RequestID: "raw", Raw: []byte("raw bytes\n")}, nil)
	k.InputAck(proto.Input{}, nil) // no request id: nothing to ack
	out.Close()

	type ack struct {
		RequestID string `json:"requestId"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	var acks []ack
	for _, r := range readRecords(t, path) {
		var ev struct {
			Type string `json:"type"`
			Data ack    `json:"data"`
		}
		if r.Ch == "event" && json.Unmarshal(r.Event, &ev) == nil && ev.Type == proto.EvInputAck {
			acks = append(acks, ev.Data)
		}
	}
	if len(acks) != 3 {
		t.Fatalf("got %d acks: %+v", len(acks), acks)
	}
	if acks[0].RequestID != "prompt" || acks[0].Text != "use [REDACTED:TOKEN] please" || acks[0].Truncated {
		t.Errorf("prompt ack: %+v", acks[0])
	}
	if len(acks[1].Text) != maxAckedText || !acks[1].Truncated {
		t.Errorf("big ack: %d bytes, truncated=%v", len(acks[1].Text), acks[1].Truncated)
	}
	if acks[2].Text != "raw bytes\n" {
		t.Errorf("raw ack: %+v", acks[2])
	}
}
