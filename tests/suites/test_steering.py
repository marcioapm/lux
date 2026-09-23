"""Steering that does not depend on the agent: delivery exactly once,
generic workloads (stdin, SIGINT), and input before a Run starts. What
every agent must do is in test_agents.py, run against every harness."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until






def test_retried_input_is_delivered_once(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(run_id, "idle")
    for _ in range(3):
        lux.run("steer", run_id, "echo once", "--request-id", "same")
    lux.wait_output(run_id, "once")
    lux.wait_activity(run_id, "idle")
    # All three reached luxd; the shim delivered one.
    assert len(lux.events(run_id, "input")) == 3
    wait_until(lambda: lux.events(run_id, "input.delivered"), 20, 0.3, "no delivery ack")
    assert len(lux.events(run_id, "input.delivered")) == 1
    assert lux.logs(run_id).count("once") == 1


def test_generic_input_goes_to_stdin(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(generic(fake_image, "lux-fake", "plain"))
    lux.wait_state(run_id, "running")
    lux.run("steer", run_id, "echo via-stdin")
    lux.wait_output(run_id, "via-stdin")
    lux.run("steer", run_id, "exit 4")
    assert lux.wait_state(run_id, "failed")["exitCode"] == 4


def test_generic_interrupt_is_sigint(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'echo got-int; exit 0' INT; echo ready; while :; do sleep 0.2; done"))
    lux.wait_output(run_id, "ready")
    lux.run("interrupt", run_id)
    assert lux.wait_state(run_id, "succeeded", "failed")["state"] == "succeeded"
    assert "got-int" in lux.logs(run_id)


def test_input_before_start_is_rejected(lux, fake_image):
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))  # no hosts: stays submitted
    with pytest.raises(CLIError) as e:
        lux.run("steer", run_id, "hello")
    assert "not started" in e.value.stderr






def test_fake_conditionals(lux, runners, hosts, fake_image):
    """lux-fake's if-exists / unless-exists run the rest of the line on a
    path's presence (for scripted agents that react to the tree)."""
    runners.start(hosts[0])
    script = "\n".join([
        "unless-exists FIXED.md echo finding: not fixed",
        "if-exists FIXED.md echo WRONG-1",
        "write FIXED.md done",
        "unless-exists FIXED.md echo WRONG-2",
        "if-exists FIXED.md echo fixed now",
        "if-exists /etc/os-release unless-exists /nope echo nested-ok",
    ])
    run_id = lux.submit(fake_agent(fake_image, script))
    lux.wait_activity(run_id, "idle")
    out = lux.logs(run_id)
    assert "finding: not fixed" in out and "fixed now" in out and "nested-ok" in out, out
    assert "WRONG" not in out, out
    lux.run("cancel", run_id)
