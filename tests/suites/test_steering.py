"""Step 4: steering. Input goes to a running workload through its adapter:
queued until the turn ends where the protocol has no mid-turn message
(ACP), delivered at once where it does (Claude Code), written to stdin for
generic workloads. Every input is an event, acknowledged on delivery."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until


def claude_fake(image: str, prompt: str) -> dict:
    """The claude-code adapter driving lux-fake, which speaks stream-json."""
    return {
        "image": {"ref": image},
        "workload": {"adapter": "claude-code", "command": ["lux-fake"], "prompt": prompt, "workdir": "/workspace"},
        "volumes": [
            {"name": "workspace", "path": "/workspace", "kind": "state"},
            {"name": "home", "path": "/home/agent", "kind": "state"},
        ],
    }


def events_of(lux, run_id: str, typ: str) -> list[dict]:
    return [e for e in lux.json("events", run_id) if e["type"] == typ]


def test_input_is_acknowledged_on_delivery(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(run_id, "idle")
    req = lux.run("steer", run_id, "echo steered", "--request-id", "req-1").stdout.strip()
    assert req == "req-1"
    lux.wait_output(run_id, "steered")
    delivered = wait_until(lambda: events_of(lux, run_id, "input.delivered"), 20, 0.3, "no delivery ack")
    assert delivered[0]["data"]["requestId"] == "req-1"
    assert events_of(lux, run_id, "input")[0]["data"]["text"] == "echo steered"


def test_retried_input_is_delivered_once(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(run_id, "idle")
    for _ in range(3):
        lux.run("steer", run_id, "echo once", "--request-id", "same")
    lux.wait_output(run_id, "once")
    time.sleep(2)
    assert lux.logs(run_id).count("once") == 1


def test_acp_queues_input_during_a_turn(lux, runners, hosts, fake_image):
    """ACP has no mid-turn message: input sent during a turn runs after it."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo first\nsleep 3\necho first-done"))
    lux.wait_output(run_id, "first")
    assert lux.get(run_id)["activity"] == "busy"
    lux.run("steer", run_id, "echo second")
    out = lux.wait_output(run_id, "second")
    assert out.index("first-done") < out.index("second"), out


def test_acp_interrupt_cancels_the_turn(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo long\nsleep 60\necho never"))
    lux.wait_output(run_id, "long")
    lux.run("steer", run_id, "echo instead", "--interrupt")
    out = lux.wait_output(run_id, "instead", timeout=20)
    assert "cancelled" in out and "never" not in out, out


def test_waiting_for_input_is_visible(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo done"))
    lux.wait_activity(run_id, "idle")
    assert "waiting for input" in lux.run("ls").stdout


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
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'echo got-int; exit 0' INT; while :; do sleep 0.2; done"))
    lux.wait_state(run_id, "running")
    time.sleep(1)
    lux.run("interrupt", run_id)
    assert lux.wait_state(run_id, "succeeded", "failed")["state"] == "succeeded"
    assert "got-int" in lux.logs(run_id)


def test_input_before_start_is_rejected(lux, fake_image):
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))  # no hosts: stays submitted
    with pytest.raises(CLIError) as e:
        lux.run("steer", run_id, "hello")
    assert "not started" in e.value.stderr


def test_claude_code_adapter_with_stream_json(lux, runners, hosts, fake_image):
    """The claude-code adapter over the stream-json protocol: prompt, session
    id, native mid-turn queueing, interrupt, and --resume on another host."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(claude_fake(fake_image, "write a.txt from-a\necho turn-one\nsleep 2\necho turn-one-done"))
    lux.wait_output(run_id, "turn-one")
    # Mid-turn: delivered at once, Claude Code queues it natively.
    lux.run("steer", run_id, "echo turn-two")
    out = lux.wait_output(run_id, "turn-two")
    assert out.index("turn-one-done") < out.index("turn-two")
    run = lux.wait_activity(run_id, "idle")
    session = run["sessionId"]
    assert session.startswith("fake-")

    lux.run("steer", run_id, "sleep 60\necho never")
    time.sleep(1)
    lux.run("interrupt", run_id)
    lux.wait_activity(run_id, "idle", timeout=20)
    assert "never" not in lux.logs(run_id)

    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    lux.run("resume", run_id, "--wait", "--input", "read a.txt\nhistory")
    out = lux.wait_output(run_id, "history:")
    assert "from-a" in out
    assert "write a.txt from-a" in out[out.index("history:"):]
    assert lux.get(run_id)["sessionId"] == session


def test_claude_code_stop_is_sigint(lux, runners, hosts, fake_image):
    """Stopping Claude Code sends SIGINT (a clean turn end), not SIGTERM."""
    runners.start(hosts[0])
    run_id = lux.submit(claude_fake(fake_image, "echo started\nsleep 60"))
    lux.wait_output(run_id, "started")
    lux.run("stop", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "stopped"
    assert run["placements"][0]["exitCode"] == 130, run["placements"][0]
