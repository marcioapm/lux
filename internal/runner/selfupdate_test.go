package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// binariesMatchManifest fetches luxd's current manifest and reports a
// match only when this host's own arch is listed there with shas equal
// to what this runner already reports: the check MsgExit handling uses
// to ignore a stale, redelivered exit once the runner has already caught
// up (whichever restart or undrain got there first).
func TestBinariesMatchManifest(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	var manifest map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runner/bin/manifest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(manifest)
	}))
	defer srv.Close()

	r := &Runner{runnerSHA256: "r1", shimSHA256: "s1"}
	r.api = newAPI(srv.URL, "tok", "host")
	ctx := context.Background()

	manifest = map[string]map[string]string{arch: {"lux-runner": "r1", "lux-shim": "s1"}}
	if !r.binariesMatchManifest(ctx) {
		t.Error("matching shas for this arch did not match")
	}

	manifest = map[string]map[string]string{arch: {"lux-runner": "r2", "lux-shim": "s1"}}
	if r.binariesMatchManifest(ctx) {
		t.Error("a differing runner sha matched")
	}

	manifest = map[string]map[string]string{"linux-riscv64": {"lux-runner": "r1", "lux-shim": "s1"}}
	if r.binariesMatchManifest(ctx) {
		t.Error("a manifest without this arch matched")
	}

	manifest = nil
	if r.binariesMatchManifest(ctx) {
		t.Error("an empty manifest matched")
	}
}

// A manifest luxd cannot be reached for never matches: MsgExit must still
// be acted on rather than silently swallowed by a network blip.
func TestBinariesMatchManifestUnreachable(t *testing.T) {
	r := &Runner{runnerSHA256: "r1", shimSHA256: "s1"}
	r.api = newAPI("http://127.0.0.1:1", "tok", "host") // nothing listens
	if r.binariesMatchManifest(context.Background()) {
		t.Error("an unreachable luxd matched")
	}
}

// shouldExit refuses to act on a MsgExit while the host holds a live
// placement (a fresh assign landed before the stale exit did), even if
// its binaries are outdated: never risk exiting mid-Run.
func TestShouldExitHoldsForLivePlacements(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]map[string]string{arch: {"lux-runner": "old", "lux-shim": "old"}})
	}))
	defer srv.Close()

	r := &Runner{
		runnerSHA256: "old", shimSHA256: "old",
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		placements: map[string]*placement{"run1": {phase: "running"}},
	}
	r.api = newAPI(srv.URL, "tok", "host")
	if r.shouldExit(context.Background(), proto.ExitHost{Code: 42}) {
		t.Error("shouldExit returned true while a placement is live")
	}
}

// shouldExit refuses to act on a MsgExit once this runner's own binaries
// already match what luxd's manifest currently advertises: the exit is
// stale (this host already caught up by some other path).
func TestShouldExitHoldsWhenBinariesAlreadyMatch(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]map[string]string{arch: {"lux-runner": "r1", "lux-shim": "s1"}})
	}))
	defer srv.Close()

	r := &Runner{
		runnerSHA256: "r1", shimSHA256: "s1",
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		placements: map[string]*placement{},
	}
	r.api = newAPI(srv.URL, "tok", "host")
	if r.shouldExit(context.Background(), proto.ExitHost{Code: 42}) {
		t.Error("shouldExit returned true although this runner's binaries already match the manifest")
	}
}

// The ordinary case: no live placements, binaries genuinely outdated.
func TestShouldExitProceedsWhenIdleAndOutdated(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]map[string]string{arch: {"lux-runner": "new", "lux-shim": "new"}})
	}))
	defer srv.Close()

	r := &Runner{
		runnerSHA256: "old", shimSHA256: "old",
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		placements: map[string]*placement{},
	}
	r.api = newAPI(srv.URL, "tok", "host")
	if !r.shouldExit(context.Background(), proto.ExitHost{Code: 42}) {
		t.Error("shouldExit returned false for a genuinely outdated, idle host")
	}
}
