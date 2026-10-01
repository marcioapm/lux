package spec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheckAttachmentsTypes(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	ok := map[string][]byte{
		"image/png":  []byte("\x89PNG\r\n\x1a\nrest"),
		"image/jpeg": {0xff, 0xd8, 0xff, 0xdb},
		"image/gif":  []byte("GIF89a..."),
		"image/webp": []byte("RIFF\x10\x00\x00\x00WEBPVP8 "),
	}
	for typ, b := range ok {
		if errs := CheckAttachments("a", []Attachment{{Name: "x", ContentType: typ, Data: enc(b)}}); errs != nil {
			t.Errorf("%s refused: %v", typ, errs)
		}
		if got := SniffImage(b); got != typ {
			t.Errorf("SniffImage %s = %q", typ, got)
		}
	}
	errs := CheckAttachments("a", []Attachment{{Name: "x", ContentType: "image/webp", Data: enc([]byte("RIFF\x10\x00\x00\x00WAVE"))}})
	if len(errs) != 1 || !strings.Contains(errs[0], "a[0]: contentType image/webp does not match its bytes (not an image lux takes)") {
		t.Fatalf("got %v", errs)
	}
	// Unpadded base64 is not standard base64 (13 bytes: padded with ==).
	errs = CheckAttachments("a", []Attachment{{Name: "x", ContentType: "image/png", Data: strings.TrimRight(enc([]byte("\x89PNG\r\n\x1a\nrest!")), "=")}})
	if len(errs) != 1 || !strings.Contains(errs[0], "not standard base64") {
		t.Fatalf("got %v", errs)
	}
}

func TestAttachmentMeta(t *testing.T) {
	a := Attachment{Name: "n.gif", ContentType: "image/gif", Data: base64.StdEncoding.EncodeToString([]byte("GIF89a"))}
	m := a.Meta()
	sum := sha256.Sum256([]byte("GIF89a"))
	want := AttachmentMeta{Name: "n.gif", ContentType: "image/gif", Size: 6, SHA256: hex.EncodeToString(sum[:])}
	if m != want {
		t.Fatalf("%+v", m)
	}
}

func TestInputsDir(t *testing.T) {
	home := Volume{Name: "home", Path: "/home/agent", Kind: "state"}
	ws := Volume{Name: "workspace", Path: "/workspace", Kind: "state"}
	cache := Volume{Name: "cache", Path: "/cache", Kind: "ephemeral"}
	codexVol := Volume{Name: "codex", Path: "/home/agent/.codex", Kind: "state"}
	repoAtHome := &Git{Repositories: []Repository{{Name: "dots", Path: "/home/agent"}}}
	for _, c := range []struct {
		name, adapter string
		vols          []Volume
		git           *Git
		dir, root     string
	}{
		{"agent home volume", "claude-code", []Volume{ws, home}, nil, "/home/agent/.lux-inputs", "/home/agent"},
		{"session volume of its own", "codex", []Volume{ws, home, codexVol}, nil, "/home/agent/.codex/.lux-inputs", "/home/agent/.codex"},
		{"acp: the home's", "acp", []Volume{ws, home}, nil, "/home/agent/.lux-inputs", "/home/agent"},
		{"acp: the first state volume", "acp", []Volume{cache, ws}, nil, "/workspace/.lux-inputs", "/workspace"},
		{"checkout over the home", "claude-code", []Volume{home, ws}, repoAtHome, "/workspace/.lux-inputs", "/workspace"},
		{"checkout over every state volume", "claude-code", []Volume{home}, repoAtHome, RuntimeInputsDir, "/.lux/run"},
		{"no state volume", "acp", []Volume{cache}, nil, RuntimeInputsDir, "/.lux/run"},
	} {
		s := RunSpec{Workload: Workload{Adapter: c.adapter}, Volumes: c.vols, Git: c.git}
		if dir, root := s.InputsDir("/home/agent"); dir != c.dir || root != c.root {
			t.Errorf("%s: %s on %s, want %s on %s", c.name, dir, root, c.dir, c.root)
		}
	}
}
