"""Scheduling: where Runs go, why they wait, and what they may touch."""

from __future__ import annotations

import time

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_unplaceable_runs_do_not_block_the_queue(lux, runners, hosts):
    """Runs no host can take must not hide the ones behind them."""
    runners.start(hosts[0])
    blocked = [lux.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"gpu": "yes"}}))
               for _ in range(25)]
    ok = lux.submit(generic(ALPINE_IMAGE, "echo", "placed"))
    assert lux.wait_state(ok, "succeeded", "failed", timeout=60)["state"] == "succeeded"
    # And the waiting ones say why, with the reason committed.
    run = lux.get(blocked[0])
    assert run["state"] == "submitted"
    assert run["stateReason"] == "no host matches", run


def test_drain_only_touches_the_callers_host(tenant_factory, runners, hosts, env):
    """Two tenants each have a host called host-x; draining one must not
    stop the other's Runs."""
    from conftest import Runners
    a, b = tenant_factory(), tenant_factory()
    ra, rb = Runners(env, a), Runners(env, b)
    try:
        # Both register as host-x, each in its own tenant.
        ra.start(hosts[0], name="host-x")
        rb.start(hosts[1], name="host-x")
        run_b = b.submit(generic(ALPINE_IMAGE, "sleep", "300"))
        b.wait_state(run_b, "running")
        a.run("hosts", "drain", "host-x")
        time.sleep(3)
        assert b.get(run_b)["state"] == "running", "tenant A's drain stopped tenant B's Run"
        b.run("cancel", run_b)
    finally:
        ra.stop_all()
        rb.stop_all()


def test_drain_moves_runs_elsewhere(lux, runners, hosts, fake_image):
    from conftest import fake_agent
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "write f.txt moved\necho ready"))
    lux.wait_output(run_id, "ready")
    lux.wait_activity(run_id, "idle")
    runners.start(hosts[1])
    lux.run("hosts", "drain", hosts[0].name)
    # Stopped with reason drain, then automatically resumed on the other host.
    run = wait_until(lambda: (lambda r: r if len(r["placements"]) == 2 and r["state"] == "running" else None)(lux.get(run_id)),
                     90, 0.5, "drained Run never resumed elsewhere")
    assert [p["hostName"] for p in run["placements"]] == [hosts[0].name, hosts[1].name], run["placements"]
    assert run["placements"][0]["stopReason"] == "drain"
    lux.run("steer", run_id, "read f.txt")
    lux.wait_output(run_id, "moved")
    hs = {h["name"]: h for h in lux.json("hosts", "ls")}
    assert hs[hosts[0].name]["draining"]
    assert hs[hosts[0].name]["times"]["drainRequested"]


def test_cancel_right_after_submit_is_not_lost(lux, runners, hosts):
    """A cancel that reaches the runner together with its assign must still
    stop the Run."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    lux.run("cancel", run_id)
    run = lux.wait_state(run_id, "cancelled", timeout=60)
    assert run["state"] == "cancelled"


def test_resumed_failed_run_is_not_finished(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "test -f /d/second && sleep 300; touch /d/second; exit 1",
                   volumes=[{"name": "d", "path": "/d", "kind": "state"}])
    run_id = lux.submit(spec)
    assert lux.wait_state(run_id, "failed")["finishedAt"]
    lux.run("resume", run_id)
    run = lux.wait_state(run_id, "running")
    assert not run.get("finishedAt"), run
    lux.run("cancel", run_id)


def test_resume_retry_keeps_secrets(lux, runners, hosts):
    """A second resume while the first is pending must not wipe the secret
    values the first supplied."""
    spec = generic(ALPINE_IMAGE, "sh", "-c", 'echo "len=${#TOKEN}"; sleep 300',
                   secrets=[{"name": "TOKEN", "value": "abcdef123456"}])
    run_id = lux.submit(spec)
    lux.run("stop", run_id)  # stops before any host exists
    assert lux.get(run_id)["state"] == "stopped"
    lux.run("resume", run_id, "--secret", "TOKEN=abcdef123456")
    # Retried without secrets: an idempotent no-op that keeps the first's.
    lux.run("resume", run_id)
    runners.start(hosts[0])
    lux.wait_output(run_id, "len=12")
    lux.run("cancel", run_id)
