package adapter

import (
	"encoding/json"
	"testing"
)

type streamSink struct {
	Sink
	typ, key, text string
	wrap           func(string) any
}

func (s *streamSink) Stream(typ, key, text string, wrap func(string) any) {
	s.typ, s.key, s.text, s.wrap = typ, key, text, wrap
}

// Streamed text goes to Stream with the event's shape kept: the key leaves
// out the text and volatile fields, and wrap puts the released text back.
func TestStreamEvent(t *testing.T) {
	var s streamSink
	raw := `{"uuid":"u1","event":{"delta":{"type":"text_delta","text":"hel"},"index":1}}`
	if !streamEvent(&s, "claude.stream_event", "", []byte(raw), [][]string{{"event", "delta", "thinking"}, {"event", "delta", "text"}}, "uuid") {
		t.Fatal("not streamed")
	}
	if s.typ != "claude.stream_event" || s.text != "hel" || s.key != `{"event":{"delta":{"text":"","type":"text_delta"},"index":1}}` {
		t.Fatalf("got %+v", s)
	}
	out, _ := json.Marshal(s.wrap("hello"))
	if string(out) != `{"event":{"delta":{"text":"hello","type":"text_delta"},"index":1},"uuid":"u1"}` {
		t.Fatalf("wrapped: %s", out)
	}
	if streamEvent(&s, "x", "", []byte(`{"event":{"delta":{"type":"input_json_delta"}}}`), [][]string{{"event", "delta", "text"}}) {
		t.Fatal("streamed an event without text")
	}
}
