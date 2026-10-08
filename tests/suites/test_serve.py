"""The dev environment (`run_tests.py --serve`): each test brings up its own,
detached, with one host, as serve.py does, and takes it down."""

from __future__ import annotations

import pytest

import serve as serve_module
from conftest import Lux, generic
from env import ALPINE_IMAGE, TestEnvironment


@pytest.fixture
def served(env, require, tmp_path, monkeypatch):
    require("luxd", "lux-runner", "lux-shim", "lux")
    # Not the developer's record of their own detached environment.
    monkeypatch.setattr(serve_module, "LAST", tmp_path / "lux-dev-env.json")
    made = []

    def up(nested: bool) -> Lux:
        e = TestEnvironment(n_hosts=1)
        e.binaries, e.extra["images"] = env.binaries, env.extra.get("images", {})
        made.append(e)
        serve_module.serve(e, env.fake_image, detach=True, nested=nested)
        return Lux(e, e.api_key, e.tenant_id)
    yield up
    for e in made:
        e.teardown()


def nested_run(lux: Lux) -> str:
    return lux.submit(generic(ALPINE_IMAGE, "echo", "placed", sandbox={"nestedContainers": True}))


def test_serve_nested_offers_nested_containers(served):
    lux = served(nested=True)
    (host,) = lux.json("hosts", "ls")
    assert host["labels"].get("nested") == "true", host
    run_id = nested_run(lux)
    lux.wait_state(run_id, "succeeded", timeout=60)
    assert lux.get(run_id)["placements"][0]["hostName"] == host["name"]


def test_plain_serve_does_not(served):
    lux = served(nested=False)
    (host,) = lux.json("hosts", "ls")
    assert host["labels"].get("nested") != "true", host
    run_id = nested_run(lux)
    lux.wait_state(lux.submit(generic(ALPINE_IMAGE, "echo", "plain")), "succeeded")
    assert lux.get(run_id)["state"] in ("submitted", "provisioning"), lux.get(run_id)
