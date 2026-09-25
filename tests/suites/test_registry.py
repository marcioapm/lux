"""Private registries: the runner logs in with runner-only credentials
(image.registryAuth), a shared build cache in a registry
(image.build.cache), and lux removing the images it pulled once unused."""

from __future__ import annotations

import json
import uuid
from pathlib import Path

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def events(lux, run_id: str, typ: str) -> list[dict]:
    return [e["data"] for e in lux.events(run_id, typ)]


def exists(host, ref: str) -> bool:
    return "yes" in host.exec("sh", "-c", f"podman image exists {ref} && echo yes || echo no")


def private(registry, image_ref: str, auth: bool = True, **extra) -> dict:
    spec = generic(image_ref, "echo", "hello from the registry", **extra)
    if auth:
        spec["secrets"] = [{"name": "REGISTRY_CREDS", "value": registry.creds}]
        spec["image"]["registryAuth"] = [{"registry": registry.address, "secret": "REGISTRY_CREDS"}]
    return spec


def assert_nowhere(lux, env, run_id: str, secret: str):
    """The secret is in none of what lux shows or logs about the Run."""
    seen = {
        "get": json.dumps(lux.get(run_id)),
        "get (text)": lux.run("get", run_id).stdout,
        "events": json.dumps(lux.json("events", run_id)),
        "logs": lux.run("logs", run_id, check=False).stdout,
        "luxd.log": (Path(env.log_dir) / "luxd.log").read_text(errors="replace"),
    }
    for h in env.hosts:
        log = Path(h.log_dir) / "runner.log"
        if log.exists():
            seen[f"{h.name} runner.log"] = log.read_text(errors="replace")
    for where, text in seen.items():
        assert secret not in text, f"the registry password is in {where}"


def test_pull_from_a_private_registry(lux, env, runners, hosts, registry):
    host = runners.start(hosts[0])
    ref = registry.push(host, ALPINE_IMAGE, f"private/alpine:{uuid.uuid4().hex[:8]}")

    # Without credentials the registry refuses the pull.
    denied = lux.submit(private(registry, ref, auth=False))
    run = lux.wait_state(denied, "failed")
    assert "image" in run["stateReason"], run
    assert any(w in run["stateReason"].lower() for w in ("unauthorized", "authentication required")), run

    ok = lux.submit(private(registry, ref, workload={"adapter": "generic", "command": [
        "sh", "-c", "echo hello from the registry; env; cat /.lux/secrets/* 2>/dev/null; true"]}))
    lux.wait_state(ok, "succeeded")
    out = lux.logs(ok)
    assert "hello from the registry" in out
    assert registry.password not in out, "the workload saw the registry credentials"
    assert [e["ref"] for e in events(lux, ok, "image.pull")] == [ref]
    secrets = {s["name"]: s for s in lux.get(ok)["spec"]["secrets"]}
    assert secrets["REGISTRY_CREDS"]["as"] == "none" and secrets["REGISTRY_CREDS"]["runnerOnly"], secrets
    # The auth file is gone once the image is there.
    assert host.exec("sh", "-c", "ls /var/lib/lux/tmp").split() == []

    # Wrong credentials: refused, and the wrong password is not echoed.
    wrong = "wrongpw-" + uuid.uuid4().hex
    bad = private(registry, registry.push(host, ALPINE_IMAGE, f"private/other:{uuid.uuid4().hex[:8]}"))
    bad["secrets"][0]["value"] = f"{registry.user}:{wrong}"
    bad_id = lux.submit(bad)
    run = lux.wait_state(bad_id, "failed")
    assert "unauthorized" in run["stateReason"].lower() or "authentication" in run["stateReason"].lower(), run

    for run_id in (denied, ok):
        assert_nowhere(lux, env, run_id, registry.password)
    assert_nowhere(lux, env, bad_id, wrong)


def test_the_build_cache_is_shared_between_hosts(lux, env, runners, hosts, registry):
    """Run A builds on host A and pushes to the cache; the same spec on
    host B pulls it instead of building: the same image."""
    a, b = hosts[0], hosts[1]
    repo = f"cache/{lux.tenant_id.lower()}"
    marker = uuid.uuid4().hex
    # Pinned by digest: each Run pins an unpinned FROM on its first host,
    # and the key covers the pinned form. (A host that pushed alpine records
    # the registry's digest for it, so the hosts could pin it differently.)
    digest = a.podman("image", "inspect", "--format", "{{.Digest}}", ALPINE_IMAGE).strip()
    spec = generic(ALPINE_IMAGE, "cat", "/marker")
    spec["image"] = {
        "build": {"containerfile": f"FROM {ALPINE_IMAGE}@{digest}\nRUN echo {marker} > /marker\n",
                  "cache": f"{registry.address}/{repo}"},
        "registryAuth": [{"registry": registry.address, "secret": "REGISTRY_CREDS"}],
    }
    spec["secrets"] = [{"name": "REGISTRY_CREDS", "value": registry.creds}]

    runners.start(a)
    first = lux.submit(spec)
    lux.wait_state(first, "succeeded")
    assert marker in lux.logs(first)
    assert events(lux, first, "image.build"), "host A did not build"
    pushed = wait_until(lambda: [e for e in events(lux, first, "image.cache") if e.get("pushed")], 60, 0.5,
                        "no image.cache pushed event")[0]
    key = events(lux, first, "image.built")[0]["tag"].split(":")[-1]
    assert pushed["ref"] == f"{registry.address}/{repo}:{key}", pushed
    assert key in registry.tags(repo)
    runners.stop(a)

    runners.start(b)
    assert not exists(b, f"localhost/lux-build:{key}")
    second = lux.submit(spec)
    lux.wait_state(second, "succeeded")
    assert marker in lux.logs(second)
    hits = [e for e in events(lux, second, "image.cache") if e.get("hit")]
    assert hits and hits[0]["ref"] == pushed["ref"], events(lux, second, "image.cache")
    assert not events(lux, second, "image.build"), "host B built instead of using the cache"
    assert lux.get(second)["image"]["imageId"] == lux.get(first)["image"]["imageId"]
    for run_id in (first, second):
        assert_nowhere(lux, env, run_id, registry.password)


def test_pulled_images_are_removed_after_the_host_ttl(lux, runners, hosts, registry):
    """An image lux pulled goes once unused for the host TTL; images the
    host had before (the preloaded alpine) stay."""
    host = hosts[0]
    ref = registry.push(host, ALPINE_IMAGE, f"private/ttl:{uuid.uuid4().hex[:8]}")
    runners.start(host, "--host-ttl", "5s")
    run_id = lux.submit(private(registry, ref))
    lux.wait_state(run_id, "succeeded")
    assert events(lux, run_id, "image.pull")
    # A Run on the preloaded image: used, but not lux's to remove.
    other = lux.submit(generic(ALPINE_IMAGE, "true"))
    lux.wait_state(other, "succeeded")
    assert exists(host, ref)
    wait_until(lambda: not exists(host, ref), 60, 1, f"{ref} was not removed")
    assert exists(host, ALPINE_IMAGE)
    assert "private/ttl" not in host.exec("cat", "/var/lib/lux/images.json")


def test_a_build_from_a_private_base(lux, env, runners, hosts, registry):
    """FROM a private image: pinning pulls it with the Run's registryAuth."""
    host = runners.start(hosts[0])
    ref = registry.push(host, ALPINE_IMAGE, f"private/base:{uuid.uuid4().hex[:8]}")
    spec = generic(ALPINE_IMAGE, "cat", "/from-private")
    spec["image"] = {"build": {"containerfile": f"FROM {ref}\nRUN echo ok > /from-private\n"},
                     "registryAuth": [{"registry": registry.address, "secret": "REGISTRY_CREDS"}]}
    spec["secrets"] = [{"name": "REGISTRY_CREDS", "value": registry.creds}]
    run_id = lux.submit(spec)
    lux.wait_state(run_id, "succeeded")
    assert "ok" in lux.logs(run_id)
    assert [e["ref"] for e in events(lux, run_id, "image.pull")] == [ref]
    assert f"FROM {ref}@sha256:" in lux.get(run_id)["image"]["containerfile"]
    assert_nowhere(lux, env, run_id, registry.password)


def test_a_private_image_is_not_reused_by_another_tenant(env, tenant_factory, runners, hosts, registry):
    """Images are host-wide, but pulling one is proof of access: on a
    shared platform host, tenant B can't run tenant A's privately pulled
    image just because it is already there; B's Run pulls it as itself,
    and without credentials that is refused."""
    tag = uuid.uuid4().hex[:8]
    pool = f"shared-{tag}"
    env.luxd_admin("create-pool", "--name", pool, "--provider", "static", "--shared")
    token = env.luxd_admin("create-host-token", "--pool", pool)["token"]
    host = runners.start(hosts[0], token=token, name=f"plat-{tag}")
    ref = registry.push(host, ALPINE_IMAGE, f"private/shared:{tag}")

    a, b = tenant_factory(), tenant_factory()
    run_a = a.submit(private(registry, ref, placement={"pool": pool}))
    a.wait_state(run_a, "succeeded")
    assert exists(host, ref), "A's pull did not leave the image on the host"

    run_b = b.submit(private(registry, ref, auth=False, placement={"pool": pool}))
    run = b.wait_state(run_b, "failed")
    assert any(w in run["stateReason"].lower() for w in ("unauthorized", "authentication required")), run
    assert "hello from the registry" not in b.logs(run_b)

    # With its own credentials, B may.
    run_b2 = b.submit(private(registry, ref, placement={"pool": pool}))
    b.wait_state(run_b2, "succeeded")
