package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// runnerArches are the architectures luxd may hold runner binaries for.
// Others are simply never offered.
var runnerArches = []string{"arm64", "amd64"}

// runnerBinNames are the files expected under runner_bin_dir/linux-<arch>/.
var runnerBinNames = []string{"lux-runner", "lux-shim"}

// loadRunnerBinaries hashes (sha256) whatever runner_bin_dir holds, once at
// startup: arch/linux-arm64/lux-runner and lux-shim, and the amd64
// equivalents. A missing arch directory or file is simply not offered; the
// binaries themselves are never re-read after this (a new release needs a
// restart, which is also how a drained host's replacement runs the new
// build).
func (s *Server) loadRunnerBinaries() {
	bins := map[string]map[string]string{}
	if s.cfg.RunnerBinDir == "" {
		s.bins = bins
		return
	}
	for _, arch := range runnerArches {
		for _, name := range runnerBinNames {
			path := filepath.Join(s.cfg.RunnerBinDir, "linux-"+arch, name)
			sum, err := sha256File(path)
			if err != nil {
				continue
			}
			if bins[arch] == nil {
				bins[arch] = map[string]string{}
			}
			bins[arch][name] = sum
		}
	}
	s.bins = bins
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// outdatedBinariesReason is the drain reason luxd uses when a host's
// runner or shim no longer match runner_bin_dir; also read back to
// recognize a host drained for exactly this (so a heartbeat never
// re-drains it, and a matching restart can be un-drained).
const outdatedBinariesReason = "outdated binaries"

// binariesOutdated reports whether luxd holds a binary for arch that
// differs from what the runner reports having. Only checked for shas the
// runner sends and luxd holds: an older runner (no shas) or an arch luxd
// serves nothing for are never grounds to drain.
func (s *Server) binariesOutdated(arch, runnerSHA, shimSHA string) bool {
	have := s.bins[arch]
	if len(have) == 0 {
		return false
	}
	if runnerSHA != "" && have["lux-runner"] != "" && have["lux-runner"] != runnerSHA {
		return true
	}
	if shimSHA != "" && have["lux-shim"] != "" && have["lux-shim"] != shimSHA {
		return true
	}
	return false
}

// binariesMatch is the converse, used to un-drain a host once a restart
// downloaded binaries that now match: both shas must be present and
// neither outdated (a runner still reporting nothing never un-drains
// itself this way; it stays drained until it does).
func (s *Server) binariesMatch(arch, runnerSHA, shimSHA string) bool {
	return runnerSHA != "" && shimSHA != "" && !s.binariesOutdated(arch, runnerSHA, shimSHA)
}

func (s *Server) runnerBinSHA256(arch, name string) (string, bool) {
	sum, ok := s.bins[arch][name]
	return sum, ok
}

// runnerBinManifest is the contract's {"linux-arm64": {"lux-runner": sha,
// "lux-shim": sha}, ...}, for arches luxd holds anything for.
func (s *Server) runnerBinManifest() map[string]map[string]string {
	out := map[string]map[string]string{}
	for arch, m := range s.bins {
		if len(m) > 0 {
			out["linux-"+arch] = m
		}
	}
	return out
}

// serveRunnerBinManifest is GET /runner/bin/manifest: host-token auth, like
// the other /runner/... routes.
func (s *Server) serveRunnerBinManifest(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.authHostToken(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.runnerBinManifest())
	return nil
}

// serveRunnerBin is GET /runner/bin/linux-{arch}/{lux-runner|lux-shim}:
// streams the binary (~20MB), with its length and sha256 (X-Lux-Sha256) so
// the downloader can verify it without a second round trip.
func (s *Server) serveRunnerBin(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.authHostToken(r); err != nil {
		return err
	}
	osArch := r.PathValue("osArch")
	name := r.PathValue("name")
	arch, ok := strings.CutPrefix(osArch, "linux-")
	if !ok || (name != "lux-runner" && name != "lux-shim") {
		return errNotFound
	}
	sum, ok := s.runnerBinSHA256(arch, name)
	if !ok {
		return errNotFound
	}
	f, err := os.Open(filepath.Join(s.cfg.RunnerBinDir, osArch, name))
	if err != nil {
		return errNotFound
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("X-Lux-Sha256", sum)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("runner binary download cut short", "arch", arch, "name", name, "err", err)
		panic(http.ErrAbortHandler) // abort the response: never a clean end
	}
	return nil
}
