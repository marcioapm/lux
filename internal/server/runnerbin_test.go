package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRunnerBinaries(t *testing.T) {
	dir := t.TempDir()
	write := func(arch, name, content string) {
		t.Helper()
		d := filepath.Join(dir, "linux-"+arch)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(content string) string {
		h := sha256.Sum256([]byte(content))
		return hex.EncodeToString(h[:])
	}
	write("arm64", "lux-runner", "runner-arm64")
	write("arm64", "lux-shim", "shim-arm64")
	write("amd64", "lux-runner", "runner-amd64")
	// amd64's lux-shim is missing: that arch is still offered, just
	// without it (the manifest only lists what is there).

	s := &Server{cfg: Config{RunnerBinDir: dir}}
	s.loadRunnerBinaries()

	got, ok := s.runnerBinSHA256("arm64", "lux-runner")
	if !ok || got != sum("runner-arm64") {
		t.Errorf("arm64 lux-runner: %q %v", got, ok)
	}
	if _, ok := s.runnerBinSHA256("amd64", "lux-shim"); ok {
		t.Error("amd64 lux-shim: found one that was never written")
	}
	if _, ok := s.runnerBinSHA256("riscv64", "lux-runner"); ok {
		t.Error("an arch luxd never heard of was offered")
	}

	m := s.runnerBinManifest()
	if m["linux-arm64"]["lux-runner"] != sum("runner-arm64") || m["linux-arm64"]["lux-shim"] != sum("shim-arm64") {
		t.Errorf("manifest linux-arm64: %+v", m["linux-arm64"])
	}
	if m["linux-amd64"]["lux-runner"] != sum("runner-amd64") {
		t.Errorf("manifest linux-amd64: %+v", m["linux-amd64"])
	}
	if _, has := m["linux-amd64"]["lux-shim"]; has {
		t.Error("manifest listed an amd64 lux-shim that does not exist")
	}
}

// An empty runner_bin_dir (the default when nothing is configured to
// serve): every lookup misses, cleanly.
func TestLoadRunnerBinariesEmptyDir(t *testing.T) {
	s := &Server{cfg: Config{RunnerBinDir: ""}}
	s.loadRunnerBinaries()
	if len(s.runnerBinManifest()) != 0 {
		t.Errorf("manifest not empty: %+v", s.runnerBinManifest())
	}
	if _, ok := s.runnerBinSHA256("arm64", "lux-runner"); ok {
		t.Error("found a binary with no configured directory")
	}
}

// binariesOutdated only fires when luxd holds a binary for the arch that
// differs from what the runner reports; an older runner (no shas) or an
// arch luxd serves nothing for are never grounds to drain.
func TestBinariesOutdated(t *testing.T) {
	s := &Server{bins: map[string]map[string]string{
		"arm64": {"lux-runner": "r1", "lux-shim": "s1"},
	}}
	cases := []struct {
		name               string
		arch, runner, shim string
		want               bool
	}{
		{"matches", "arm64", "r1", "s1", false},
		{"runner differs", "arm64", "r2", "s1", true},
		{"shim differs", "arm64", "r1", "s2", true},
		{"older runner: no shas", "arm64", "", "", false},
		{"unknown arch: luxd holds nothing", "amd64", "whatever", "whatever", false},
	}
	for _, c := range cases {
		if got := s.binariesOutdated(c.arch, c.runner, c.shim); got != c.want {
			t.Errorf("%s: binariesOutdated = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBinariesMatch(t *testing.T) {
	s := &Server{bins: map[string]map[string]string{
		"arm64": {"lux-runner": "r1", "lux-shim": "s1"},
	}}
	if !s.binariesMatch("arm64", "r1", "s1") {
		t.Error("matching shas did not match")
	}
	if s.binariesMatch("arm64", "r2", "s1") {
		t.Error("a differing runner sha matched")
	}
	if s.binariesMatch("arm64", "", "") {
		t.Error("empty shas (no report yet) matched")
	}
}
