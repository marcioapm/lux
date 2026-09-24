"""Step: luxd serving runner binaries for self-update (runner_bin_dir): a
runner downloads /runner/bin/linux-{arch}/{lux-runner,lux-shim} and checks
its sha256 against /runner/bin/manifest and the X-Lux-Sha256 header."""

from __future__ import annotations

import hashlib
import os
from pathlib import Path

import pytest
import requests


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
    d, _, _ = runner_bin_dir
    for path in ("/runner/bin/manifest", "/runner/bin/linux-arm64/lux-runner"):
        resp = requests.get(f"{env.luxd_url}{path}", timeout=10)
        assert resp.status_code == 401, (path, resp.status_code)


def test_a_missing_arch_or_binary_is_not_offered(env, runner_bin_dir):
    _, token, content = runner_bin_dir
    manifest = _get(env, "/runner/bin/manifest", token).json()
    assert "linux-riscv64" not in manifest
    resp = _get(env, "/runner/bin/linux-riscv64/lux-runner", token)
    assert resp.status_code == 404
