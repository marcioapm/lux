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
	for sc.Scan() {
		var r proto.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
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
