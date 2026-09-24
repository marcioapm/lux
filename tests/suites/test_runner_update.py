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
    draining for something else (e.g. a person asked for it)."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    lux.run("hosts", "drain", host.name)
    wait_until(lambda: lux.json("hosts", "get", host.name)["draining"], 15, 0.3, "manual drain never took")
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin3", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        time.sleep(3)
        h = lux.json("hosts", "get", host.name)
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
