"""Step 13: EC2 pools. luxd launches hosts when Runs wait for them, keeps
warm and minimum hosts, drains and terminates idle ones, and replaces
hosts EC2 lost. Against a fake EC2 (the real provider code, a fake API);
--real-ec2 runs them against AWS."""

from __future__ import annotations

import json

import pytest

import conftest
from conftest import fake_only, generic
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


@pytest.fixture(autouse=True)
def _clean(lux, ec2):
    """Each test's pool is removed after it, and its instances with it."""
    yield
    lux.run("pools", "rm", "burst", check=False)
    wait_until(lambda: not ec2.running(), 90, 1, "the removed pool's instances were not terminated")


def pool(lux, name="burst", **kw):
    args = ["pools", "set", name, "--provider", "ec2", "--template", json.dumps(conftest.EC2_TEMPLATE)]
    for k, v in kw.items():
        args += [f"--{k}", str(v)]
    lux.run(*args)


def ec2_hosts(lux, pool_name="burst", states=("ready",)):
    return [h for h in lux.json("hosts", "ls") if h["pool"] == pool_name and h["state"] in states]


def test_a_waiting_run_gets_a_host_launched(lux, ec2):
    pool(lux, max=2)
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "on-ec2", placement={"pool": "burst"}))
    wait_until(lambda: lux.get(run_id)["state"] in ("provisioning", "scheduled", "running", "succeeded"),
               30, 0.5, "the Run never waited for a host")
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert "on-ec2" in lux.logs(run_id)
    [inst] = ec2.running()
    assert inst["launchTemplate"] == conftest.EC2_TEMPLATE["launchTemplate"]
    assert inst["tags"]["lux:pool"] == "burst"
    host = lux.get(run_id)["placements"][0]["hostName"]
    assert host == inst["tags"]["Name"], (host, inst["tags"])
    # Idle past the scale-down delay: drained, then terminated.
    wait_until(lambda: not ec2.running(), 60, 1, "the idle host was never terminated")
    wait_until(lambda: not ec2_hosts(lux, states=("ready", "draining")), 30, 1, "the host is still listed")


def test_max_hosts_is_respected(lux, ec2):
    fake_only(ec2)
    pool(lux, max=1)
    runs = [lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "sleep 3", placement={"pool": "burst"},
                               resources={"cpus": 0.5})) for _ in range(3)]
    for r in runs:
        lux.wait_state(r, "succeeded", timeout=180)
    assert ec2.calls.count("RunInstances") == 1, ec2.calls


def test_warm_and_minimum_hosts(lux, ec2):
    """A warm host is launched with nothing waiting, and a Run starts on it
    at once; the minimum is kept after scale-down."""
    fake_only(ec2)
    pool(lux, min=1, warm=1, max=3)
    wait_until(lambda: len(ec2_hosts(lux)) >= 1, 90, 1, "no warm host")
    before = ec2.calls.count("RunInstances")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "warm", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=60)
    run = lux.get(run_id)
    assert run["placements"][0]["hostName"] in {h["name"] for h in ec2_hosts(lux, states=("ready", "draining"))}
    # Scaling down never goes below the minimum (1) and warm (1).
    wait_until(lambda: len(ec2.running()) == 1 and ec2.calls.count("RunInstances") >= before, 60, 1, "not back to one host")
    import time
    time.sleep(8)
    assert len(ec2.running()) == 1


def test_a_lost_instance_is_replaced(lux, ec2):
    pool(lux, min=1, max=2)
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 1, "no host")
    [inst] = ec2.running()
    ec2.kill(inst["id"])
    wait_until(lambda: [i for i in ec2.running() if i["id"] != inst["id"]], 90, 1, "no replacement launched")
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 1, "the replacement never registered")


def test_a_host_that_never_registers_is_terminated(lux, ec2):
    fake_only(ec2)
    ec2.no_boot = True
    pool(lux, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") == 1, 30, 0.5, "never launched")
    # LUX_LAUNCH_TIMEOUT=60s in this fixture.
    wait_until(lambda: "TerminateInstances" in ec2.calls, 120, 1, "the stuck host was never terminated")
    ec2.no_boot = False
    lux.wait_state(run_id, "succeeded", timeout=180)


def test_a_failed_launch_is_retried(lux, ec2):
    fake_only(ec2)
    ec2.fail_launches = True
    pool(lux, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") >= 2, 30, 0.5, "not retried")
    ec2.fail_launches = False
    lux.wait_state(run_id, "succeeded", timeout=120)


def test_a_busy_host_is_drained_before_it_is_terminated(lux, ec2):
    """Removing the pool never cuts a Run short: its host is drained (the
    Run stops and snapshots), and terminated only once that snapshot is
    uploaded — never while the Run is still live on it."""
    pool(lux, max=1)
    # Slow to stop (a grace period it uses in full), so a terminate that
    # does not wait for the drain would catch it live.
    script = "trap 'sleep 6; exit 0' TERM; echo state > /w/f; echo up; while :; do sleep 1; done"
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"},
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}])
    spec["workload"]["grace"] = "30s"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "up", timeout=120)
    lux.run("pools", "rm", "burst")
    # While the Run stops, its instance is still there.
    wait_until(lambda: lux.get(run_id)["state"] == "stopping", 30, 0.3, "never asked to stop")
    assert ec2.running(), "terminated while its Run was still stopping"
    wait_until(lambda: not ec2.running(), 90, 1, "never terminated")
    snaps = lux.json("snapshots", run_id)
    assert snaps and snaps[-1]["uploaded"], snaps
    run = lux.get(run_id)
    assert run["placements"][-1]["exitReason"] != "lost", run
