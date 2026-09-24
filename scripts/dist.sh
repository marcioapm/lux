#!/usr/bin/env bash
# dist.sh builds the release tarballs the contract names, plus SHA256SUMS,
# into dist/. Called by `make dist VERSION=vX.Y.Z`.
#
#   lux_<version>_linux_{arm64,amd64}.tar.gz   bin/{luxd,lux}, lib/lux/runner/linux-{arm64,amd64}/{lux-runner,lux-shim}
#   lux_<version>_darwin_{arm64,amd64}.tar.gz  bin/lux (CLI only)
#
# Both runner arches go in every linux tarball (arm64 and amd64), so any
# luxd unpacked from either serves both to runner hosts without a second
# download. Unpacking a tarball into /usr/local gives the default layout
# (runner_bin_dir defaults to /usr/local/lib/lux/runner).
set -euo pipefail

VERSION="${VERSION:?set VERSION=vX.Y.Z}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
LDFLAGS="-s -w -X github.com/marcioapm/lux/internal/version.Version=$VERSION"

rm -rf "$DIST"
mkdir -p "$DIST"

build() {
  local goos=$1 goarch=$2 pkg=$3 out=$4
  echo "building $pkg for $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$LDFLAGS" -o "$out" "./cmd/$pkg"
}

# One work tree per linux arch: bin/ for luxd and lux, lib/lux/runner/ for
# both runner arches' lux-runner and lux-shim.
for arch in arm64 amd64; do
  work="$DIST/work-linux-$arch"
  mkdir -p "$work/bin" "$work/lib/lux/runner/linux-arm64" "$work/lib/lux/runner/linux-amd64"
  build linux "$arch" luxd "$work/bin/luxd"
  build linux "$arch" lux "$work/bin/lux"
done
# The runner and shim binaries are the same for both linux tarballs
# (every arch's build goes in both): build each runner arch once, copy
# into both work trees.
for rarch in arm64 amd64; do
  build linux "$rarch" lux-runner "$DIST/lux-runner-linux-$rarch"
  build linux "$rarch" lux-shim "$DIST/lux-shim-linux-$rarch"
  for arch in arm64 amd64; do
    cp "$DIST/lux-runner-linux-$rarch" "$DIST/work-linux-$arch/lib/lux/runner/linux-$rarch/lux-runner"
    cp "$DIST/lux-shim-linux-$rarch" "$DIST/work-linux-$arch/lib/lux/runner/linux-$rarch/lux-shim"
  done
done
rm -f "$DIST"/lux-runner-linux-* "$DIST"/lux-shim-linux-*

for arch in arm64 amd64; do
  chmod +x "$DIST/work-linux-$arch/bin/"* "$DIST/work-linux-$arch/lib/lux/runner"/*/*
  tar -C "$DIST/work-linux-$arch" -czf "$DIST/lux_${VERSION}_linux_${arch}.tar.gz" bin lib
  rm -rf "$DIST/work-linux-$arch"
done

# darwin: the CLI only.
for arch in arm64 amd64; do
  work="$DIST/work-darwin-$arch"
  mkdir -p "$work/bin"
  build darwin "$arch" lux "$work/bin/lux"
  chmod +x "$work/bin/lux"
  tar -C "$work" -czf "$DIST/lux_${VERSION}_darwin_${arch}.tar.gz" bin
  rm -rf "$work"
done

(cd "$DIST" && sha256sum lux_*.tar.gz > SHA256SUMS)

echo "dist/:"
ls -la "$DIST"
