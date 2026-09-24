package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
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

// hasBothBinaries reports whether luxd holds every binary in
// runnerBinNames for arch: the precondition for drain and undrain
// decisions, so luxd is never asking a host for what it cannot itself
// serve (a partial upload, or a build that only produced one file).
func (s *Server) hasBothBinaries(arch string) bool {
	have := s.bins[arch]
	for _, name := range runnerBinNames {
		if have[name] == "" {
			return false
		}
	}
	return true
}

// binariesOutdated reports whether luxd holds a binary for arch that
// differs from what the runner reports having. Only when luxd holds both
// binaries for the arch (hasBothBinaries): an older runner (no shas), an
// arch luxd serves nothing for, or one where only one of the pair is
// present are never grounds to drain — luxd never drains a host for an
// arch it cannot itself serve.
func (s *Server) binariesOutdated(arch, runnerSHA, shimSHA string) bool {
	if !s.hasBothBinaries(arch) {
		return false
	}
	have := s.bins[arch]
	if runnerSHA != "" && have["lux-runner"] != runnerSHA {
		return true
	}
	if shimSHA != "" && have["lux-shim"] != shimSHA {
		return true
	}
	return false
}

// binariesMatch is the converse, used to un-drain a host once a restart
// downloaded binaries that now match: luxd must hold both binaries for the
// arch, both shas must be present and neither outdated (a runner still
// reporting nothing never un-drains itself this way; it stays drained
// until it does).
func (s *Server) binariesMatch(arch, runnerSHA, shimSHA string) bool {
	return s.hasBothBinaries(arch) && runnerSHA != "" && shimSHA != "" && !s.binariesOutdated(arch, runnerSHA, shimSHA)
}

// outdatedDrainRoom reports whether hostID's pool has room for one more
// concurrent outdated-binaries drain, capped at
// max(1, OutdatedDrainPercent% of the pool's live hosts): a release must
// not cordon a whole pool's worth of capacity in one instant. A pool's
// live hosts and its currently-draining-for-this-reason hosts are counted
// together (coalesce(tenant_id,”), pool): the same grouping a pool's
// hosts share.
func (s *Server) outdatedDrainRoom(ctx context.Context, tx pgx.Tx, hostID string) (bool, error) {
	var live, draining int
	err := tx.QueryRow(ctx, `
		WITH h AS (SELECT coalesce(tenant_id, '') AS tenant, pool FROM hosts WHERE id = $1)
		SELECT
			count(*) FILTER (WHERE state IN ('ready', 'draining')),
			count(*) FILTER (WHERE draining AND state_reason = $2)
		FROM hosts, h
		WHERE coalesce(hosts.tenant_id, '') = h.tenant AND hosts.pool = h.pool`,
		hostID, outdatedBinariesReason).Scan(&live, &draining)
	if err != nil {
		return false, err
	}
	room := max(1, live*s.cfg.OutdatedDrainPercent/100)
	return draining < room, nil
}

func (s *Server) runnerBinSHA256(arch, name string) (string, bool) {
	sum, ok := s.bins[arch][name]
	return sum, ok
}

// runnerBinManifest is the contract's {"linux-arm64": {"lux-runner": sha,
// "lux-shim": sha}, ...}, for arches luxd holds both binaries for: a host
// downloading by this manifest, or a drain decision made from it, never
// meets an arch luxd can only half serve.
func (s *Server) runnerBinManifest() map[string]map[string]string {
	out := map[string]map[string]string{}
	for arch := range s.bins {
		if s.hasBothBinaries(arch) {
			out["linux-"+arch] = s.bins[arch]
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
