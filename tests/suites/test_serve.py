"""The dev environment (`run_tests.py --serve`): each test brings up its own,
detached, with two hosts, as serve.py does, and takes it down."""

from __future__ import annotations

import pytest

import serve as serve_module
from build import NESTED_IMAGE, build_nested_images
from conftest import Lux, generic
from env import ALPINE_IMAGE, TestEnvironment


@pytest.fixture
def served(env, require, tmp_path, monkeypatch):
    require("luxd", "lux-runner", "lux-shim", "lux")
    # Not the developer's record of their own detached environment.
    monkeypatch.setattr(serve_module, "LAST", tmp_path / "lux-dev-env.json")
    made = []

    def up(nested: bool) -> Lux:
        e = TestEnvironment(n_hosts=2)
        # Only the image a nested Run here uses: each image is exported and
        # loaded into every host again.
        e.binaries, e.extra["images"] = env.binaries, {"nested": nested_image()} if nested else {}
        made.append(e)
        serve_module.serve(e, env.fake_image, detach=True, nested=nested)
        return Lux(e, e.api_key, e.tenant_id)

    def nested_image() -> str:
        return env.extra.get("images", {}).get("nested") or build_nested_images(podman=True, docker=False)[0]
    yield up
    for e in made:
        e.teardown()


def nested_run(lux: Lux, **extra) -> str:
    # Starts no inner container: it shows the Run is placed and its image is
    # on the host, not that Podman works inside.
    return lux.submit(generic(NESTED_IMAGE, "echo", "placed", sandbox={"nestedContainers": True}, **extra))


def test_serve_nested_offers_nested_containers(served):
    lux = served(nested=True)
    hosts = lux.json("hosts", "ls")
    assert len(hosts) == 2, hosts
    for host in hosts:
        assert host["state"] == "ready" and host["labels"].get("nested") == "true", host
    # One Run pinned to each host (its runner-set name label), all at once.
    runs = {nested_run(lux, placement={"requires": {"name": h["name"]}}): h["name"] for h in hosts}
    for run_id, name in runs.items():
        lux.wait_state(run_id, "succeeded", timeout=60)
        assert lux.get(run_id)["placements"][0]["hostName"] == name
        assert lux.logs(run_id).strip() == "placed"


def test_plain_serve_does_not(served):
    lux = served(nested=False)
    for host in lux.json("hosts", "ls"):
        assert host["labels"].get("nested") != "true", host
    run_id = nested_run(lux)
    lux.wait_state(lux.submit(generic(ALPINE_IMAGE, "echo", "plain")), "succeeded")
    assert lux.get(run_id)["state"] in ("submitted", "provisioning"), lux.get(run_id)
