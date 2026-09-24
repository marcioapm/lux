package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// runnerArches are the architectures luxd may hold runner binaries for.
// Others are simply never offered.
var runnerArches = []string{"arm64", "amd64"}

// runnerBinNames are the files expected under runner_bin_dir/linux-<arch>/.
var runnerBinNames = []string{"lux-runner", "lux-shim"}

// runnerBin is one binary read into memory at startup: its bytes and their
// sha256, together, so serving one can never disagree with the other.
type runnerBin struct {
	data   []byte
	sha256 string
}

// loadRunnerBinaries reads whatever runner_bin_dir holds into memory, once
// at startup: arch/linux-arm64/lux-runner and lux-shim, and the amd64
// equivalents, hashed as they are read. A missing arch directory or file
// is simply not offered. Serving from these bytes (serveRunnerBin), rather
// than reopening the path on every request, means a release that replaces
// the files on disk in place can never make luxd serve bytes that don't
// match the sha256 it already told a host: the new build only takes
// effect on luxd's own restart, same as any other config.
func (s *Server) loadRunnerBinaries() {
	bins := map[string]map[string]runnerBin{}
	if s.cfg.RunnerBinDir == "" {
		s.bins = bins
		return
	}
	for _, arch := range runnerArches {
		for _, name := range runnerBinNames {
			path := filepath.Join(s.cfg.RunnerBinDir, "linux-"+arch, name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if bins[arch] == nil {
				bins[arch] = map[string]runnerBin{}
			}
			bins[arch][name] = runnerBin{data: data, sha256: sha256Hex(data)}
		}
	}
	s.bins = bins
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
		if _, ok := have[name]; !ok {
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
	if runnerSHA != "" && have["lux-runner"].sha256 != runnerSHA {
		return true
	}
	if shimSHA != "" && have["lux-shim"].sha256 != shimSHA {
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
// together (coalesce(tenant_id, ”), pool): the same grouping a pool's
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
	b, ok := s.bins[arch][name]
	return b.sha256, ok
}

// runnerBinManifest is the contract's {"linux-arm64": {"lux-runner": sha,
// "lux-shim": sha}, ...}, for arches luxd holds both binaries for: a host
// downloading by this manifest, or a drain decision made from it, never
// meets an arch luxd can only half serve.
func (s *Server) runnerBinManifest() map[string]map[string]string {
	out := map[string]map[string]string{}
	for arch, m := range s.bins {
		if !s.hasBothBinaries(arch) {
			continue
		}
		shas := map[string]string{}
		for name, b := range m {
			shas[name] = b.sha256
		}
		out["linux-"+arch] = shas
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
// serves the binary (~20MB) held in memory since startup (loadRunnerBinaries),
// never reopening the path — a release that replaces the file on disk in
// place can never make this serve bytes whose sha256 differs from what it
// advertises, in X-Lux-Sha256 and the manifest, for the process's life.
func (s *Server) serveRunnerBin(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.authHostToken(r); err != nil {
		return err
	}
	osArch := r.PathValue("osArch")
	name := r.PathValue("name")
	arch, ok := strings.CutPrefix(osArch, "linux-")
	if !ok || !slices.Contains(runnerBinNames, name) {
		return errNotFound
	}
	b, ok := s.bins[arch][name]
	if !ok {
		return errNotFound
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(b.data)))
	w.Header().Set("X-Lux-Sha256", b.sha256)
	if _, err := io.Copy(w, bytes.NewReader(b.data)); err != nil {
		s.log.Warn("runner binary download cut short", "arch", arch, "name", name, "err", err)
		panic(http.ErrAbortHandler) // abort the response: never a clean end
	}
	return nil
}
