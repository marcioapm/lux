// Package hostboot renders what a runner host needs to boot: the
// environment lux-runner starts with, the systemd unit that runs it, and
// the script that downloads its binaries and verifies them against luxd
// (internal/server's /runner/bin/... contract). One source of truth for
// the unit and the fetch script, rendered for EC2 pools (internal/ec2's
// template.userData: ignition, script, env) and for GET
// /runner/bootstrap.sh (static hosts).
package hostboot

import (
	"fmt"
	"strings"
)

// Env is what a runner needs to reach luxd and register. URL and
// HostToken are the only ones every launch needs; HostName defaults to
// the hostname when empty (a static host, or a launch that left it out);
// EC2IMDS is empty outside EC2.
type Env struct {
	URL       string
	HostToken string
	HostName  string
	EC2IMDS   string
}

// pairs are Env's fields as KEY, value, only the ones set: the order
// every rendering (and a human reading one) can rely on.
func (e Env) pairs() [][2]string {
	var out [][2]string
	add := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	add("LUX_URL", e.URL)
	add("LUX_HOST_TOKEN", e.HostToken)
	add("LUX_HOST_NAME", e.HostName)
	add("LUX_EC2_IMDS", e.EC2IMDS)
	return out
}

// Lines is the "env" userData format: KEY=value lines, for a custom AMI
// whose own boot script reads them (this is what every EC2 launch sent
// before self-update and Ignition existed; kept for AMIs already built
// around it). Also /etc/lux/runner.env's content for the ignition and
// script formats: lux-runner and the fetch script both read it with
// EnvironmentFile=/os.Getenv, whichever wrote it.
func (e Env) Lines() string {
	var b strings.Builder
	for _, kv := range e.pairs() {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.String()
}

// UnitName is the systemd unit every format installs.
const UnitName = "lux-runner.service"

// Unit is lux-runner.service. EnvironmentFile supplies LUX_URL,
// LUX_HOST_TOKEN, LUX_HOST_NAME, LUX_EC2_IMDS, which lux-runner reads
// directly (cmd/lux-runner/main.go); LUX_PROVIDER_ID is appended to the
// same file by ExecStartPre once it has it (EC2's instance id, resolved
// through LUX_EC2_IMDS — static hosts never get one, so lux-runner starts
// without --provider-id, as today). ExecStartPre re-downloads the
// binaries on every start, including the restart after an exit luxd asked
// for: the running binary is never patched in place.
const Unit = `[Unit]
Description=lux runner
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/lux/runner.env
ExecStartPre=` + FetchBinariesPath + `
ExecStart=/usr/local/lib/lux/lux-runner --data-dir /var/lib/lux --shim /usr/local/lib/lux/lux-shim
Restart=always
RestartSec=2s

[Install]
WantedBy=multi-user.target
`

// FetchBinariesPath is where every format installs FetchBinariesScript.
const FetchBinariesPath = "/usr/local/lib/lux/fetch-binaries.sh"

// FetchBinariesScript is lux-runner.service's ExecStartPre: downloads
// lux-runner and lux-shim for uname -m from LUX_URL/runner/bin/..., a
// host-token bearer request, verifying each against its X-Lux-Sha256
// header before installing it; then, if LUX_EC2_IMDS is set and the env
// file has no LUX_PROVIDER_ID yet, resolves the instance id through it
// (IMDSv2: a session token first) and appends it. Idempotent: reruns
// re-download (cheap next to a Run) and never re-append a provider id
// already recorded.
const FetchBinariesScript = `#!/bin/bash
set -euo pipefail

arch=$(uname -m)
case "$arch" in
  aarch64|arm64) larch=arm64 ;;
  x86_64|amd64)  larch=amd64 ;;
  *) echo "lux: unsupported architecture $arch" >&2; exit 1 ;;
esac

mkdir -p /usr/local/lib/lux
for bin in lux-runner lux-shim; do
  url="$LUX_URL/runner/bin/linux-$larch/$bin"
  tmp=$(mktemp)
  headers=$(mktemp)
  curl -fsS -H "Authorization: Bearer $LUX_HOST_TOKEN" -D "$headers" -o "$tmp" "$url"
  want=$(tr -d '\r' < "$headers" | awk -F': ' 'BEGIN{IGNORECASE=1} tolower($1)=="x-lux-sha256"{print tolower($2)}')
  got=$(sha256sum "$tmp" | awk '{print $1}')
  rm -f "$headers"
  if [ -z "$want" ] || [ "$got" != "$want" ]; then
    echo "lux: sha256 mismatch downloading $bin (got $got, want ${want:-<none>})" >&2
    rm -f "$tmp"
    exit 1
  fi
  install -m 0755 "$tmp" "/usr/local/lib/lux/$bin"
  rm -f "$tmp"
done

if [ -n "${LUX_EC2_IMDS:-}" ] && ! grep -q '^LUX_PROVIDER_ID=' /etc/lux/runner.env 2>/dev/null; then
  token=$(curl -fsS -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' "$LUX_EC2_IMDS/latest/api/token")
  id=$(curl -fsS -H "X-aws-ec2-metadata-token: $token" "$LUX_EC2_IMDS/latest/meta-data/instance-id")
  printf 'LUX_PROVIDER_ID=%s\n' "$id" >> /etc/lux/runner.env
fi
`

// SubuidRange is the range the runner's --userns=auto allocates from (see
// docs/operations.md "Host requirements"); appended to /etc/subuid and
// /etc/subgid, never replacing what a distro already put there.
const SubuidRange = "containers:2147483647:2147483648"

// ValidUserData reports whether v is a recognized template.userData value
// ("" defaults to "ignition"): checked when a pool is set, so a typo is
// refused there rather than surfacing as a boot failure no one is
// watching for.
func ValidUserData(v string) bool {
	switch v {
	case "", "ignition", "script", "env":
		return true
	}
	return false
}
