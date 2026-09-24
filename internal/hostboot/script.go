package hostboot

import "strings"

// scriptBody is shared between two renderings: userData "script" (cloud-init
// runs it on a stock EC2 image, with the env exported ahead of it — see
// Script) and GET /runner/bootstrap.sh (a static host, unfilled: it reads
// the env from its own environment, and defaults the host name to
// `hostname`). It never assumes Fedora CoreOS: it checks the host
// requirements from docs/operations.md and only installs what is missing,
// with dnf if present, else apt-get, else it fails loudly (the intent is a
// stock Fedora Cloud, Ubuntu, Debian or AL2023 image that already has
// everything, so the common case installs nothing).
//
// Idempotent: every write is safe to repeat, and the unit is started only
// if not already running, so a rerun (a reboot, a second manual run) never
// disturbs a live lux-runner. Output goes to stdout/stderr, which cloud-init
// and a manual `sh` both send to the console and the journal.
const scriptBody = `#!/bin/bash
[ -n "${BASH_VERSION:-}" ] || { echo "lux: run this script with bash, not sh (curl ... | sudo env ... bash)" >&2; exit 1; }
set -euo pipefail

: "${LUX_URL:?LUX_URL is required}"
: "${LUX_HOST_TOKEN:?LUX_HOST_TOKEN is required}"
LUX_HOST_NAME="${LUX_HOST_NAME:-$(hostname)}"
LUX_EC2_IMDS="${LUX_EC2_IMDS:-}"

echo "lux: bootstrapping $LUX_HOST_NAME against $LUX_URL"

missing=()
command -v podman >/dev/null 2>&1 || missing+=(podman)
command -v nft >/dev/null 2>&1 || missing+=(nftables)
command -v git >/dev/null 2>&1 || missing+=(git)
[ -f /sys/fs/cgroup/cgroup.controllers ] || { echo "lux: cgroup v2 is not mounted; this kernel/boot config cannot run lux-runner" >&2; exit 1; }
if command -v podman >/dev/null 2>&1; then
  podman_major=$(podman version --format '{{.Client.Version}}' 2>/dev/null | cut -d. -f1)
  if [ -z "$podman_major" ] || [ "$podman_major" -lt 5 ]; then
    missing+=(podman)
  fi
fi

if [ "${#missing[@]}" -gt 0 ]; then
  echo "lux: installing missing packages: ${missing[*]}"
  if command -v dnf >/dev/null 2>&1; then
    dnf -y install podman netavark aardvark-dns nftables git shadow-utils
  elif command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y podman netavark aardvark-dns nftables git uidmap
  else
    echo "lux: missing ${missing[*]} and neither dnf nor apt-get is available; install them and rerun" >&2
    exit 1
  fi
fi

mkdir -p /etc/lux
umask 077
{
  printf 'LUX_URL=%s\n' "$LUX_URL"
  printf 'LUX_HOST_TOKEN=%s\n' "$LUX_HOST_TOKEN"
  printf 'LUX_HOST_NAME=%s\n' "$LUX_HOST_NAME"
  [ -n "$LUX_EC2_IMDS" ] && printf 'LUX_EC2_IMDS=%s\n' "$LUX_EC2_IMDS"
} > /etc/lux/runner.env
chmod 0600 /etc/lux/runner.env

cat > ` + FetchBinariesPath + ` <<'LUX_FETCH_SCRIPT'
` + FetchBinariesScript + `LUX_FETCH_SCRIPT
chmod 0755 ` + FetchBinariesPath + `

cat > /etc/systemd/system/` + UnitName + ` <<'LUX_UNIT'
` + Unit + `LUX_UNIT

systemctl daemon-reload
systemctl enable ` + UnitName + `
systemctl is-active --quiet ` + UnitName + ` || systemctl start ` + UnitName + `
echo "lux: bootstrap complete"
`

// Script renders the userData "script" format: scriptBody with the env
// exported ahead of it, so cloud-init's execution needs nothing from the
// instance environment.
func Script(env Env) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	for _, kv := range env.pairs() {
		b.WriteString("export ")
		b.WriteString(kv[0])
		b.WriteString("=")
		b.WriteString(shellQuote(kv[1]))
		b.WriteString("\n")
	}
	// scriptBody already starts with its own #!/bin/bash; drop it once we
	// have our own shebang plus the exports above.
	b.WriteString(strings.TrimPrefix(scriptBody, "#!/bin/bash\n"))
	return b.String()
}

// Bootstrap renders GET /runner/bootstrap.sh: the same script, unfilled —
// it takes LUX_URL, LUX_HOST_TOKEN, LUX_HOST_NAME and LUX_EC2_IMDS from
// whatever environment it is run with (curl ... | sudo env LUX_HOST_TOKEN=...
// LUX_URL=... bash), and defaults the host name to `hostname`. No auth: it
// carries no secret. The script uses bash arrays and set -o pipefail, so
// its first line (after the guard, before either) refuses to run under a
// bare `sh`: on Debian and Ubuntu that is dash, which a piped `| sh`
// silently ignores the #!/bin/bash shebang for.
func Bootstrap() string { return scriptBody }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
