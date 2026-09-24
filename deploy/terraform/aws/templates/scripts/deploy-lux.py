#!/usr/bin/env python3
"""Deploys the lux version named in an SSM parameter.

Run by lux-deploy.timer every 5 minutes. Reads LUX_VERSION_PARAMETER; if it
differs from the installed version (kept in <install_root>/CURRENT_VERSION),
downloads lux_<version>_linux_<arch>.tar.gz and SHA256SUMS from the GitHub
release at LUX_REPO, verifies the tarball's checksum, and extracts it into a
versioned directory. `luxd migrate` is run with *that* version's binary
directly, before anything is switched over: if it fails, the running
version is untouched. Only once migrate succeeds do the `current` and
stable-path symlinks move to the new version and luxd restart; the restart
is then polled (systemctl is-active plus an HTTP health check) for about
30s. If that doesn't come up healthy, the previous symlink targets are
restored, luxd is restarted again, and the script exits non-zero — a bad
release never runs unmigrated, and a failure never leaves the box without
a working luxd. Idempotent: running it again when nothing changed does
nothing.

stdlib only, per policy: no pip install at boot.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

INSTALL_ROOT = "/usr/local/lux"
BIN_DIR = "/usr/local/bin"
RUNNER_BIN_DIR = "/usr/local/lib/lux/runner"
BIN_TARGETS = ["bin/luxd", "bin/lux"]
RUNNER_ARCHES = ["linux-arm64", "linux-amd64"]
HEALTH_URL = "http://127.0.0.1:7070/health"
HEALTH_TIMEOUT_S = 30
HEALTH_POLL_INTERVAL_S = 2


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


def installed_version(install_root: str) -> str:
    path = os.path.join(install_root, "CURRENT_VERSION")
    if not os.path.exists(path):
        return ""
    with open(path) as f:
        return f.read().strip()


def write_installed_version(install_root: str, version: str) -> None:
    with open(os.path.join(install_root, "CURRENT_VERSION"), "w") as f:
        f.write(version + "\n")


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


def extract_release(tarball_path: str, versions_root: str, version: str) -> str:
    """Extracts the tarball into <versions_root>/<version>, atomically.

    Extracts into a fresh temp directory first, then os.replace()s it into
    place: a version directory is either absent or complete, never
    partially written, and a stale leftover from a previous failed attempt
    at the same version is removed before, not after, the new one lands.
    Never touches any other version directory, so it's safe to call even
    while an older version's directory is still what `current` resolves
    to.
    """
    os.makedirs(versions_root, exist_ok=True)
    version_dir = os.path.join(versions_root, version)
    tmp_extract = tempfile.mkdtemp(prefix=f"{version}.", dir=versions_root)
    try:
        with tarfile.open(tarball_path) as tf:
            tf.extractall(tmp_extract, filter="data")
    except Exception:
        shutil.rmtree(tmp_extract, ignore_errors=True)
        raise
    if os.path.lexists(version_dir):
        shutil.rmtree(version_dir)
    os.replace(tmp_extract, version_dir)
    return version_dir


def _atomic_symlink(target: str, link_path: str) -> None:
    os.makedirs(os.path.dirname(link_path), exist_ok=True)
    tmp = link_path + ".new"
    if os.path.lexists(tmp):
        os.remove(tmp)
    os.symlink(target, tmp)
    os.replace(tmp, link_path)


def stable_links(current_link: str, bin_dir: str, runner_bin_dir: str) -> dict:
    """Maps each stable path the rest of the system points at (systemd
    units, luxd.toml's runner_bin_dir) to where it should point inside
    `current`, so a version switch is one set of symlink updates."""
    return {
        os.path.join(bin_dir, "luxd"): os.path.join(current_link, "bin/luxd"),
        os.path.join(bin_dir, "lux"): os.path.join(current_link, "bin/lux"),
        runner_bin_dir: os.path.join(current_link, "lib/lux/runner"),
    }


def switch_symlinks(version_dir: str, current_link: str, links: dict) -> dict:
    """Points `current` and every stable link at the new version.
    Returns each link's previous target (None if it didn't exist), for
    restore_symlinks to roll back to."""
    previous = {
        current_link: os.readlink(current_link) if os.path.islink(current_link) else None,
    }
    _atomic_symlink(version_dir, current_link)
    for link_path, target in links.items():
        previous[link_path] = os.readlink(link_path) if os.path.islink(link_path) else None
        _atomic_symlink(target, link_path)
    return previous


def restore_symlinks(previous: dict, current_link: str, links: dict) -> None:
    """Undoes switch_symlinks: every link goes back to what it pointed at
    before, or is removed if it didn't exist before (a first-ever deploy
    that fails its health check has nothing to roll back to)."""
    for link_path in [current_link, *links]:
        target = previous.get(link_path)
        if target is not None:
            _atomic_symlink(target, link_path)
        elif os.path.islink(link_path):
            os.remove(link_path)


def run_migrate(luxd_bin: str, dsn: str, runner=subprocess.run):
    result = runner(
        [luxd_bin, "migrate"],
        env={**os.environ, "LUX_DATABASE_URL": dsn},
        capture_output=True, text=True, check=False,
    )
    return result.returncode == 0, result.stderr


def restart_luxd(systemctl_bin: str = "systemctl", runner=subprocess.run):
    result = runner(
        [systemctl_bin, "restart", "luxd"],
        capture_output=True, text=True, check=False,
    )
    return result.returncode == 0, result.stderr


def is_luxd_active(systemctl_bin: str = "systemctl", runner=subprocess.run) -> bool:
    result = runner(
        [systemctl_bin, "is-active", "luxd"],
        capture_output=True, text=True, check=False,
    )
    return result.stdout.strip() == "active"


def is_healthy(health_url: str, urlopen=urllib.request.urlopen) -> bool:
    try:
        with urlopen(health_url, timeout=3) as resp:
            return 200 <= resp.status < 300
    except (urllib.error.URLError, OSError, ValueError):
        return False


def wait_healthy(
    check_active,
    check_health,
    timeout_s: float = HEALTH_TIMEOUT_S,
    interval_s: float = HEALTH_POLL_INTERVAL_S,
    sleep=time.sleep,
    now=time.monotonic,
) -> bool:
    """Polls check_active() and check_health() until both are true, or
    timeout_s elapses. Takes the checks as callables (not systemctl_bin /
    health_url directly) so tests can stub systemd and the HTTP call
    without touching either."""
    deadline = now() + timeout_s
    while True:
        if check_active() and check_health():
            return True
        if now() >= deadline:
            return False
        sleep(interval_s)


def main() -> None:
    repo = os.environ["LUX_REPO"]
    version_parameter = os.environ["LUX_VERSION_PARAMETER"]
    region = os.environ["LUX_REGION"]
    migrate_dsn_file = os.environ["LUX_MIGRATE_DSN_FILE"]
    health_url = os.environ.get("LUX_HEALTH_URL", HEALTH_URL)
    install_root = os.environ.get("LUX_INSTALL_ROOT", INSTALL_ROOT)
    bin_dir = os.environ.get("LUX_BIN_DIR", BIN_DIR)
    runner_bin_dir = os.environ.get("LUX_RUNNER_BIN_DIR", RUNNER_BIN_DIR)

    current_link = os.path.join(install_root, "current")
    versions_root = os.path.join(install_root, "versions")
    os.makedirs(install_root, exist_ok=True)

    wanted = get_ssm_parameter(version_parameter, region)
    if not wanted:
        fail(f"SSM parameter {version_parameter} is empty")

    current = installed_version(install_root)
    if current == wanted:
        log(f"already at {wanted}")
        return

    log(f"deploying {wanted} (currently {current or 'none'})")
    a = arch()
    tarball_name = f"lux_{wanted}_linux_{a}.tar.gz"
    base_url = f"https://github.com/{repo}/releases/download/{wanted}"

    with tempfile.TemporaryDirectory(prefix="lux-deploy-", dir=install_root) as tmp:
        tarball_path = os.path.join(tmp, tarball_name)
        sums_path = os.path.join(tmp, "SHA256SUMS")
        try:
            download(f"{base_url}/{tarball_name}", tarball_path)
            download(f"{base_url}/SHA256SUMS", sums_path)
        except Exception as e:
            fail(f"download failed, leaving {current or 'no version'} running: {e}")

        verify_sha256(tarball_path, sums_path, tarball_name)
        version_dir = extract_release(tarball_path, versions_root, wanted)

    for rel in BIN_TARGETS:
        if not os.path.exists(os.path.join(version_dir, rel)):
            fail(f"{rel} missing from {tarball_name}; leaving {current or 'no version'} running")
    for rel_arch in RUNNER_ARCHES:
        d = os.path.join(version_dir, "lib", "lux", "runner", rel_arch)
        if not os.path.isdir(d):
            log(f"note: {rel_arch} runner binaries absent from this release (expected if it targets one arch)")

    with open(migrate_dsn_file) as f:
        migrate_dsn = f.read().strip()
    new_luxd_bin = os.path.join(version_dir, "bin", "luxd")
    migrated, migrate_err = run_migrate(new_luxd_bin, migrate_dsn)
    if not migrated:
        fail(f"luxd migrate failed, leaving {current or 'no version'} running: {migrate_err.strip()}")

    links = stable_links(current_link, bin_dir, runner_bin_dir)
    previous_targets = switch_symlinks(version_dir, current_link, links)

    restarted, restart_err = restart_luxd()
    healthy = restarted and wait_healthy(
        lambda: is_luxd_active(),
        lambda: is_healthy(health_url),
    )
    if not healthy:
        log(
            f"ERROR: {wanted} did not come up healthy "
            f"(restart {'ok' if restarted else 'failed: ' + restart_err.strip()}); "
            f"rolling back to {current or 'nothing'}"
        )
        restore_symlinks(previous_targets, current_link, links)
        rolled_back, rollback_err = restart_luxd()
        if not rolled_back:
            log(f"ERROR: restart after rollback also failed: {rollback_err.strip()}")
        sys.exit(1)

    write_installed_version(install_root, wanted)
    log(f"deployed {wanted}")


if __name__ == "__main__":
    main()
