package shim

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func filesShim(t *testing.T, files ...spec.File) (*Shim, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home", "agent")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.jsonl")
	out, err := OpenOutput(path, NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	s := &Shim{out: out, red: NewRedactor(nil), user: &userInfo{uid: os.Getuid(), gid: os.Getgid(), home: home},
		cfg: proto.ShimConfig{Files: files}}
	return s, root, path
}

func dataFile(p, content string, mode spec.FileMode) spec.File {
	return spec.File{Path: p, Mode: mode, Data: base64.StdEncoding.EncodeToString([]byte(content))}
}

func readMode(t *testing.T, p string) (string, os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), fi.Mode()
}

// Every file lands with its bytes and mode, ~/ in the user's home, missing
// directories made; what was there is replaced; the shim keeps no bytes.
func TestWriteFiles(t *testing.T) {
	old := syscall.Umask(0o077) // the mode is set outright, whatever the umask
	defer syscall.Umask(old)
	s, root, out := filesShim(t)
	home := s.user.home
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "old"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Files = []spec.File{
		dataFile("~/.claude/skills/review/SKILL.md", "# review\n", 0o644),
		dataFile("~/.claude/old", "replaced", 0o600),
		dataFile(filepath.Join(root, "opt", "tool", "run.sh"), "#!/bin/sh\n", 0o755),
		{Path: "~/empty", Mode: 0o640},
	}
	if err := s.writeFiles(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]struct {
		body string
		mode os.FileMode
	}{
		filepath.Join(home, ".claude", "skills", "review", "SKILL.md"): {"# review\n", 0o644},
		filepath.Join(home, ".claude", "old"):                          {"replaced", 0o600},
		filepath.Join(root, "opt", "tool", "run.sh"):                   {"#!/bin/sh\n", 0o755},
		filepath.Join(home, "empty"):                                   {"", 0o640},
	} {
		if body, mode := readMode(t, p); body != want.body || mode != want.mode {
			t.Errorf("%s: %q %v, want %q %v", p, body, mode, want.body, want.mode)
		}
	}
	if fi, err := os.Stat(filepath.Join(home, ".claude", "skills")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("made directory: %v %v", fi.Mode(), err)
	}
	// No temporary file is left behind.
	if left, _ := filepath.Glob(filepath.Join(home, ".claude", ".lux-file-*")); len(left) > 0 {
		t.Fatal(left)
	}
	if s.cfg.Files != nil {
		t.Fatal("the shim kept the files' bytes")
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), `"type":"lux.files"`) || !strings.Contains(string(b), `"count":4`) || !strings.Contains(string(b), `"bytes":27`) {
		t.Fatalf("no lux.files event with count and bytes: %s", b)
	}
}

// Written again on the next start (a resume): a file the workload changed
// or removed is the spec's again.
func TestWriteFilesAgainOnEveryStart(t *testing.T) {
	f := dataFile("~/.claude/skills/a/SKILL.md", "v1", 0o644)
	s, _, _ := filesShim(t, f)
	p := filepath.Join(s.user.home, ".claude", "skills", "a", "SKILL.md")
	if err := s.writeFiles(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("edited by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Files = []spec.File{dataFile(f.Path, "v2", 0o644)}
	if err := s.writeFiles(); err != nil {
		t.Fatal(err)
	}
	if body, mode := readMode(t, p); body != "v2" || mode != 0o644 {
		t.Fatalf("%q %v", body, mode)
	}
}

// A link at a file's own path is replaced, and what it points to is never
// written: the workload can plant one on a state volume between starts.
func TestWriteFilesReplacesALinkNeverWritingThroughIt(t *testing.T) {
	s, root, _ := filesShim(t, dataFile("~/.bashrc", "lux's", 0o644))
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.user.home, ".bashrc")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := s.writeFiles(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("wrote through the link: %q", b)
	}
	if body, mode := readMode(t, link); body != "lux's" || !mode.IsRegular() {
		t.Fatalf("%q %v", body, mode)
	}
}

// A link among a file's directories fails the start, whichever way it
// points: lux follows none.
func TestWriteFilesRefusesALinkOnTheWay(t *testing.T) {
	s, root, _ := filesShim(t, dataFile("~/.claude/skills/x/SKILL.md", "x", 0o644))
	elsewhere := filepath.Join(root, "etc")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(s.user.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	err := s.writeFiles()
	if err == nil || !strings.Contains(err.Error(), "/.claude is a symbolic link") || !strings.HasPrefix(err.Error(), "~/.claude/skills/x/SKILL.md: ") {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) > 0 {
		t.Fatalf("wrote under the link's target: %v", entries)
	}
}

func TestWriteFilesRefusesWhatIsInTheWay(t *testing.T) {
	t.Run("a directory at the path", func(t *testing.T) {
		s, _, _ := filesShim(t, dataFile("~/d", "x", 0o644))
		if err := os.MkdirAll(filepath.Join(s.user.home, "d", "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.writeFiles(); err == nil || !strings.Contains(err.Error(), "a directory is in the way") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Stat(filepath.Join(s.user.home, "d", "keep")); err != nil {
			t.Fatal("the directory was touched")
		}
	})
	t.Run("a file where a directory goes", func(t *testing.T) {
		s, _, _ := filesShim(t, dataFile("~/f/x", "x", 0o644))
		if err := os.WriteFile(filepath.Join(s.user.home, "f"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.writeFiles(); err == nil || !strings.Contains(err.Error(), "/f is not a directory") {
			t.Fatalf("got %v", err)
		}
	})
}

// As root, the shim writes as the workload user: the files and directories
// it makes are that user's, and a directory that user cannot write is
// refused, not written as root. Needs root, as the shim always is:
//
//	docker run --rm -v $PWD:/src -w /src golang go test ./internal/shim -run WriteFilesAsTheWorkload
func TestWriteFilesAsTheWorkload(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to switch file credentials")
	}
	s, root, _ := filesShim(t, dataFile("~/.claude/skills/a/SKILL.md", "x", 0o644))
	s.user.uid, s.user.gid, s.user.groups = 1000, 1000, []uint32{1000}
	home := s.user.home
	if err := os.Chown(home, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.writeFiles(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".claude", ".claude/skills", ".claude/skills/a", ".claude/skills/a/SKILL.md"} {
		if got := lstatUID(t, filepath.Join(home, p)); got != 1000 {
			t.Errorf("%s: uid %d, want 1000", p, got)
		}
	}
	// root's directory, closed to the workload: refused.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.Files = []spec.File{dataFile(filepath.Join(locked, "x"), "x", 0o644)}
	if err := s.writeFiles(); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(locked, "x")); err == nil {
		t.Fatal("written as root")
	}
	// The shim's own threads are still root.
	if os.Geteuid() != 0 {
		t.Fatal("the shim lost root")
	}
}
