"""Step: luxd serving runner binaries for self-update (runner_bin_dir): a
runner downloads /runner/bin/linux-{arch}/{lux-runner,lux-shim} and checks
its sha256 against /runner/bin/manifest and the X-Lux-Sha256 header. Also:
luxd drains a host whose runner or shim no longer match what it holds, once,
and a static host is told to exit once it is drained and idle; a
provisioned one is terminated and replaced instead (the pool's own path)."""

from __future__ import annotations

import hashlib
import json
import os
import time

import pytest
import requests

from conftest import fake_only
from env import wait_until


@pytest.fixture
def runner_bin_dir(env, tmp_path_factory):
    """Restarts luxd with LUX_RUNNER_BIN_DIR pointing at a directory holding
    fake (but real-shaped) binaries for both arches, and yields (dir,
    host_token, sha256-by-arch-by-name)."""
    d = tmp_path_factory.mktemp("runner-bin")
    content = {}
    for arch in ("arm64", "amd64"):
        for name in ("lux-runner", "lux-shim"):
            data = f"fake {arch} {name}\n".encode() * 1000  # ~20 bytes * 1000, not 20MB: fine for a checksum test
            sub = d / f"linux-{arch}"
            sub.mkdir(exist_ok=True)
            (sub / name).write_bytes(data)
            content.setdefault(arch, {})[name] = hashlib.sha256(data).hexdigest()
    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=str(d))
    token = env.luxd_admin("create-host-token")["token"]
    yield d, token, content
    env.stop_luxd()
    env.start_luxd()


def _get(env, path, token):
    return requests.get(f"{env.luxd_url}{path}", headers={"Authorization": f"Bearer {token}"}, timeout=10)


def test_manifest_lists_every_arch_and_binary(env, runner_bin_dir):
    _, token, content = runner_bin_dir
    resp = _get(env, "/runner/bin/manifest", token)
    assert resp.status_code == 200, resp.text
    manifest = resp.json()
    for arch, files in content.items():
        for name, sha in files.items():
            assert manifest[f"linux-{arch}"][name] == sha, (arch, name, manifest)


def test_binary_download_matches_its_sha256_header_and_the_manifest(env, runner_bin_dir):
    _, token, content = runner_bin_dir
    manifest = _get(env, "/runner/bin/manifest", token).json()
    for arch, files in content.items():
        for name, want_sha in files.items():
            resp = _get(env, f"/runner/bin/linux-{arch}/{name}", token)
            assert resp.status_code == 200, resp.text
            assert resp.headers["X-Lux-Sha256"] == want_sha == manifest[f"linux-{arch}"][name]
            assert int(resp.headers["Content-Length"]) == len(resp.content)
            assert hashlib.sha256(resp.content).hexdigest() == want_sha


def test_binary_endpoints_need_a_host_token(env, runner_bin_dir):
    for path in ("/runner/bin/manifest", "/runner/bin/linux-arm64/lux-runner"):
        resp = requests.get(f"{env.luxd_url}{path}", timeout=10)
        assert resp.status_code == 401, (path, resp.status_code)


def test_a_missing_arch_or_binary_is_not_offered(env, runner_bin_dir):
    _, token, _ = runner_bin_dir
    manifest = _get(env, "/runner/bin/manifest", token).json()
    assert "linux-riscv64" not in manifest
    resp = _get(env, "/runner/bin/linux-riscv64/lux-runner", token)
    assert resp.status_code == 404


def test_an_arch_missing_one_of_the_pair_is_not_offered(env, tmp_path_factory):
    """luxd never advertises, or drains for, an arch it can only half
    serve: the manifest only lists an arch when it holds both binaries."""
    d = tmp_path_factory.mktemp("runner-bin-partial")
    (d / "linux-arm64").mkdir()
    (d / "linux-arm64" / "lux-runner").write_bytes(b"only the runner, no shim\n")
    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=str(d))
    try:
        token = env.luxd_admin("create-host-token")["token"]
        manifest = _get(env, "/runner/bin/manifest", token).json()
        assert "linux-arm64" not in manifest, manifest
        # The binary itself is still servable by name (a partial mirror is
        # not a 404), just never offered or drained for.
        resp = _get(env, "/runner/bin/linux-arm64/lux-runner", token)
        assert resp.status_code == 200, resp.text
    finally:
        env.stop_luxd()
        env.start_luxd()


# ---- bootstrap.sh (static hosts) --------------------------------------------

def test_bootstrap_script_is_served_without_auth(env):
    resp = requests.get(f"{env.luxd_url}/runner/bootstrap.sh", timeout=10)
    assert resp.status_code == 200
    assert resp.text.startswith("#!/bin/bash")
    # Shares the unit and fetch-script text with the EC2 renderings
    # (internal/hostboot): both markers must be present verbatim.
    assert "lux-runner.service" in resp.text
    assert "/runner/bin/linux-$larch/$bin" in resp.text


def test_bootstrap_script_fetches_and_verifies_binaries_on_a_host(env, hosts, runner_bin_dir):
    """Runs the actual downloaded script (not internal/hostboot's Go
    source) on a simulated host, with LUX_URL/LUX_HOST_TOKEN set as
    `curl ... | sh` would. The simulated host has no systemd PID 1, so
    `systemctl` is stubbed out: what is checked is bootstrap.sh's own
    idempotent writes (env file, unit file, fetch script) and, run
    separately exactly as the unit's ExecStartPre would, that the fetch
    script downloads and verifies both binaries against runner_bin_dir."""
    _, token, content = runner_bin_dir
    host = hosts[0]
    arch = host.exec("uname", "-m").strip()
    larch = {"aarch64": "arm64", "x86_64": "amd64"}.get(arch, arch)
    script = requests.get(f"{env.luxd_url}/runner/bootstrap.sh", timeout=10).text
    host.exec("sh", "-c", "cat > /tmp/bootstrap.sh", input=script.encode())
    host.exec("sh", "-c", "printf '#!/bin/sh\\nexit 0\\n' > /usr/local/bin/systemctl && chmod +x /usr/local/bin/systemctl")
    run = lambda: host.exec(  # noqa: E731
        "env", f"LUX_URL={env.luxd_url}", f"LUX_HOST_TOKEN={token}", "LUX_HOST_NAME=bootstrap-test",
        "bash", "/tmp/bootstrap.sh",
    )
    run()
    env_mode = host.exec("stat", "-c", "%a", "/etc/lux/runner.env").strip()
    assert env_mode == "600", env_mode
    runner_env = host.exec("cat", "/etc/lux/runner.env")
    assert f"LUX_URL={env.luxd_url}" in runner_env
    assert "LUX_HOST_NAME=bootstrap-test" in runner_env
    unit = host.exec("cat", "/etc/systemd/system/lux-runner.service")
    assert "ExecStartPre=/usr/local/lib/lux/fetch-binaries.sh" in unit
    assert "Restart=always" in unit
    fetch_mode = host.exec("stat", "-c", "%a", "/usr/local/lib/lux/fetch-binaries.sh").strip()
    assert fetch_mode == "755", fetch_mode
    subuid = host.exec("cat", "/etc/subuid")
    assert "containers:2147483647:2147483648" in subuid
    # Idempotent: a second run (a reboot) does not fail, and does not
    # duplicate the subuid/subgid line.
    run()
    assert host.exec("grep", "-c", "^containers:", "/etc/subuid").strip() == "1"

    # The fetch script it installed is exactly what lux-runner.service's
    # ExecStartPre would run, with the same EnvironmentFile= it declares:
    # exercise it the same way, as systemd would.
    host.exec("sh", "-c", "set -a; . /etc/lux/runner.env; set +a; exec bash /usr/local/lib/lux/fetch-binaries.sh")
    for name in ("lux-runner", "lux-shim"):
        got = host.exec("sha256sum", f"/usr/local/lib/lux/{name}").split()[0]
        assert got == content[larch][name], (name, got, content[larch][name])
        mode = host.exec("stat", "-c", "%a", f"/usr/local/lib/lux/{name}").strip()
        assert mode == "755", (name, mode)


# ---- draining outdated hosts ------------------------------------------------

def _host_arch(lux, name: str) -> str:
    return lux.json("hosts", "get", name)["labels"]["arch"]


def _mismatched_bin_dir(base: str, arch: str) -> str:
    """A runner_bin_dir whose binaries for arch are real files but not what
    any built lux-runner/lux-shim hashes to."""
    for name in ("lux-runner", "lux-shim"):
        d = f"{base}/linux-{arch}"
        os.makedirs(d, exist_ok=True)
        with open(f"{d}/{name}", "wb") as f:
            f.write(b"not what any real binary hashes to\n")
    return base


def test_an_outdated_static_host_is_drained_once(env, lux, runners, hosts):
    """luxd holds a binary for this host's arch that differs from what its
    runner reports: it is drained, with a clear reason, and not re-drained
    on later heartbeats (its host row's drain_requested_at does not move)."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        h = wait_until(lambda: (lambda x: x if x["draining"] else None)(lux.json("hosts", "get", host.name)),
                       30, 0.5, "the outdated host was never drained")
        assert h["stateReason"] == "outdated binaries", h
        drained_at = h["times"]["drainRequested"]

        # A later heartbeat must not re-drain it (the timestamp is stable).
        time.sleep(3)
        h2 = lux.json("hosts", "get", host.name)
        assert h2["times"]["drainRequested"] == drained_at, "re-drained on a later heartbeat"
    finally:
        env.stop_luxd()
        env.start_luxd()


def test_an_outdated_static_host_is_told_to_exit_once_drained(env, lux, runners, hosts):
    """Once an outdated, drained static host has nothing left running or to
    upload, luxd asks it to exit (a durable `exit` control message); its
    systemd unit would restart it with fresh binaries — here, the bare
    runner process (standing in for the unit's restart) simply exits."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin2", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        wait_until(lambda: (lambda x: x if x["draining"] else None)(lux.json("hosts", "get", host.name)),
                  30, 0.5, "the outdated host was never drained")
        # Idle and nothing to upload (no Runs were placed): luxd asks it to
        # exit.
        runners.procs[host.name].wait(timeout=60)
    finally:
        env.stop_luxd()
        env.start_luxd()


def test_a_host_already_draining_for_another_reason_is_left_alone(env, lux, runners, hosts):
    """Outdated binaries are never a second drain reason for a host already
    draining for something else (e.g. a person asked for it), including
    across a reconnect: the Hello that follows luxd's restart must not wipe
    the reason (`state_reason` survives while still draining)."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    lux.run("hosts", "drain", host.name)
    wait_until(lambda: lux.json("hosts", "get", host.name)["draining"], 15, 0.3, "manual drain never took")
    before_heartbeat = lux.json("hosts", "get", host.name)["lastHeartbeat"]
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin3", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        # Wait for an actual reconnect (a fresh Hello answered by the
        # restarted luxd), not a fixed sleep: a newer lastHeartbeat than
        # before luxd restarted proves the Hello that could have wiped the
        # reason has already been processed.
        wait_until(lambda: (lambda h: h if h["lastHeartbeat"] != before_heartbeat else None)(lux.json("hosts", "get", host.name)),
                   30, 0.3, "the runner never reconnected after luxd restarted")
        h = lux.json("hosts", "get", host.name)
        assert h["draining"], h
        assert h["stateReason"] == "drain requested", h  # not overwritten with "outdated binaries"
    finally:
        env.stop_luxd()
        env.start_luxd()


@pytest.mark.ec2
def test_a_provisioned_outdated_host_is_replaced(lux, ec2):
    """luxd holds runner binaries the pool's instances do not match: each
    host is drained on registering, then terminated once idle (the usual
    drained-and-empty path), and a replacement is launched — indistinguishable
    from a lost or unlisted instance being replaced."""
    fake_only(ec2)
    bin_dir = ec2.env.log_dir + "/other-bin-ec2"
    for arch in ("arm64", "amd64"):
        _mismatched_bin_dir(bin_dir, arch)
    ec2.env.stop_luxd()
    ec2.env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake",
                       AWS_REGION="us-east-1", LUX_SCALE_DOWN_AFTER="4s", LUX_LAUNCH_TIMEOUT="60s",
                       LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(ec2.template), "--min", "1", "--max", "1")
        wait_until(lambda: ec2.running(), 60, 1, "no host launched")
        [first] = ec2.running()
        wait_until(lambda: [i for i in ec2.running() if i["id"] != first["id"]], 120, 1, "no replacement launched")
    finally:
        # Stop churning (every replacement is equally "outdated" while
        # LUX_RUNNER_BIN_DIR stays mismatched) before the pool is removed.
        lux.run("pools", "rm", "burst", check=False)
        ec2.env.stop_luxd()
        ec2.env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake",
                           AWS_REGION="us-east-1", LUX_SCALE_DOWN_AFTER="4s", LUX_LAUNCH_TIMEOUT="60s")
        wait_until(lambda: not ec2.running(), 90, 1, "instances left running after the pool was removed")
