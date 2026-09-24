#!/usr/bin/env python3
"""Deploys the lux version named in an SSM parameter.

Run by lux-deploy.timer every 5 minutes. Reads LUX_VERSION_PARAMETER; if it
differs from the installed version (kept in /usr/local/lux/CURRENT_VERSION),
downloads lux_<version>_linux_<arch>.tar.gz and SHA256SUMS from the GitHub
release at LUX_REPO, verifies the tarball's checksum, unpacks it into a
versioned directory, switches the /usr/local/lux/current symlink, runs
`luxd migrate` as the database owner, and restarts luxd. On any failure it
leaves the previously installed version running and logs loudly. Idempotent:
running it again when nothing changed does nothing.

stdlib only, per policy: no pip install at boot.
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

INSTALL_ROOT = "/usr/local/lux"
CURRENT_LINK = os.path.join(INSTALL_ROOT, "current")
CURRENT_VERSION_FILE = os.path.join(INSTALL_ROOT, "CURRENT_VERSION")
BIN_TARGETS = ["bin/luxd", "bin/lux"]
RUNNER_ARCHES = ["linux-arm64", "linux-amd64"]
# The release tarball unpacks into /usr/local (bin/luxd, bin/lux,
# lib/lux/runner/...); each version is kept in its own directory instead,
# and these are the stable paths the rest of the system (systemd units,
# luxd.toml's runner_bin_dir) points at.
STABLE_LINKS = {
    "/usr/local/bin/luxd": "bin/luxd",
    "/usr/local/bin/lux": "bin/lux",
    "/usr/local/lib/lux/runner": "lib/lux/runner",
}


def log(msg: str) -> None:
    print(f"lux-deploy: {msg}", flush=True)


def fail(msg: str) -> None:
    log(f"ERROR: {msg}")
    sys.exit(1)


def arch() -> str:
    machine = os.uname().machine
    if machine in ("aarch64", "arm64"):
        return "arm64"
    if machine in ("x86_64", "amd64"):
        return "amd64"
    fail(f"unsupported architecture: {machine}")


def get_ssm_parameter(name: str, region: str) -> str:
    out = subprocess.run(
        [
            "aws", "ssm", "get-parameter",
            "--name", name,
            "--region", region,
            "--query", "Parameter.Value",
            "--output", "text",
        ],
        capture_output=True, text=True, check=False,
    )
    if out.returncode != 0:
        fail(f"reading SSM parameter {name}: {out.stderr.strip()}")
    return out.stdout.strip()


def installed_version() -> str:
    if not os.path.exists(CURRENT_VERSION_FILE):
        return ""
    with open(CURRENT_VERSION_FILE) as f:
        return f.read().strip()


def download(url: str, dest: str) -> None:
    log(f"downloading {url}")
    with urllib.request.urlopen(url, timeout=60) as resp, open(dest, "wb") as out:
        shutil.copyfileobj(resp, out)


def verify_sha256(tarball_path: str, sums_path: str, tarball_name: str) -> None:
    want = None
    with open(sums_path) as f:
        for line in f:
            parts = line.split()
            if len(parts) == 2 and parts[1].lstrip("*") == tarball_name:
                want = parts[0]
                break
    if want is None:
        fail(f"{tarball_name} not listed in SHA256SUMS")
    h = hashlib.sha256()
    with open(tarball_path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    got = h.hexdigest()
    if got != want:
        fail(f"checksum mismatch for {tarball_name}: want {want}, got {got}")


def main() -> None:
    repo = os.environ["LUX_REPO"]
    version_parameter = os.environ["LUX_VERSION_PARAMETER"]
    region = os.environ["LUX_REGION"]
    migrate_dsn_file = os.environ["LUX_MIGRATE_DSN_FILE"]

    wanted = get_ssm_parameter(version_parameter, region)
    if not wanted:
        fail(f"SSM parameter {version_parameter} is empty")

    current = installed_version()
    if current == wanted:
        log(f"already at {wanted}")
        return

    log(f"deploying {wanted} (currently {current or 'none'})")
    a = arch()
    tarball_name = f"lux_{wanted}_linux_{a}.tar.gz"
    base_url = f"https://github.com/{repo}/releases/download/{wanted}"

    with tempfile.TemporaryDirectory(prefix="lux-deploy-") as tmp:
        tarball_path = os.path.join(tmp, tarball_name)
        sums_path = os.path.join(tmp, "SHA256SUMS")
        try:
            download(f"{base_url}/{tarball_name}", tarball_path)
            download(f"{base_url}/SHA256SUMS", sums_path)
        except Exception as e:
            fail(f"download failed, leaving {current or 'no version'} running: {e}")

        verify_sha256(tarball_path, sums_path, tarball_name)

        version_dir = os.path.join(INSTALL_ROOT, "versions", wanted)
        if os.path.exists(version_dir):
            shutil.rmtree(version_dir)
        os.makedirs(version_dir, exist_ok=True)
        with tarfile.open(tarball_path) as tf:
            tf.extractall(version_dir, filter="data")

    for rel in BIN_TARGETS:
        if not os.path.exists(os.path.join(version_dir, rel)):
            fail(f"{rel} missing from {tarball_name}; leaving {current or 'no version'} running")
    for rel_arch in RUNNER_ARCHES:
        d = os.path.join(version_dir, "lib", "lux", "runner", rel_arch)
        if not os.path.isdir(d):
            log(f"note: {rel_arch} runner binaries absent from this release (expected if it targets one arch)")

    tmp_link = CURRENT_LINK + ".new"
    if os.path.lexists(tmp_link):
        os.remove(tmp_link)
    os.symlink(version_dir, tmp_link)
    os.replace(tmp_link, CURRENT_LINK)

    for link_path, rel in STABLE_LINKS.items():
        os.makedirs(os.path.dirname(link_path), exist_ok=True)
        tmp = link_path + ".new"
        if os.path.lexists(tmp):
            os.remove(tmp)
        os.symlink(os.path.join(CURRENT_LINK, rel), tmp)
        os.replace(tmp, link_path)

    with open(migrate_dsn_file) as f:
        migrate_dsn = f.read().strip()
    migrate = subprocess.run(
        ["/usr/local/bin/luxd", "migrate"],
        env={**os.environ, "LUX_DATABASE_URL": migrate_dsn},
        capture_output=True, text=True, check=False,
    )
    if migrate.returncode != 0:
        fail(
            f"luxd migrate failed, leaving {current or 'no version'} running: "
            f"{migrate.stderr.strip()}"
        )

    restart = subprocess.run(["systemctl", "restart", "luxd"], capture_output=True, text=True, check=False)
    if restart.returncode != 0:
        fail(f"systemctl restart luxd failed after migrating to {wanted}: {restart.stderr.strip()}")

    with open(CURRENT_VERSION_FILE, "w") as f:
        f.write(wanted + "\n")
    log(f"deployed {wanted}")


if __name__ == "__main__":
    main()
