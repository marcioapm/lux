"""Step 4: steering. Input goes to a running workload through its adapter:
queued until the turn ends where the protocol has no mid-turn message
(ACP), delivered at once where it does (Claude Code), written to stdin for
generic workloads. Every input is an event, acknowledged on delivery."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until




def events_of(lux, run_id: str, typ: str) -> list[dict]:
    return [e for e in lux.json("events", run_id) if e["type"] == typ]


def interrupt_then_resume_elsewhere(lux, runners, a, b, run_id: str):
    """Interrupt a long turn, stop, and resume on the other host: the file
    written on host a and the conversation both carry over."""
    session = lux.get(run_id)["sessionId"]
    lux.run("steer", run_id, "sleep 60\necho never")
    lux.wait_activity(run_id, "busy")
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
    lux.wait_activity(run_id, "idle")
    # All three reached luxd; the shim delivered one.
    assert len(events_of(lux, run_id, "input")) == 3
    wait_until(lambda: events_of(lux, run_id, "input.delivered"), 20, 0.3, "no delivery ack")
    assert len(events_of(lux, run_id, "input.delivered")) == 1
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


def test_claude_code_adapter_with_stream_json(lux, runners, hosts, fake_image):
    """The claude-code adapter over the stream-json protocol: prompt, session
    id, native mid-turn queueing, interrupt, and --resume on another host."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, adapter="claude-code", prompt="write a.txt from-a\necho turn-one\nsleep 2\necho turn-one-done"))
    lux.wait_output(run_id, "turn-one")
    # Mid-turn: delivered at once, Claude Code queues it natively.
    lux.run("steer", run_id, "echo turn-two")
    out = lux.wait_output(run_id, "turn-two")
    assert out.index("turn-one-done") < out.index("turn-two")
    assert lux.wait_activity(run_id, "idle")["sessionId"].startswith("fake-")
    interrupt_then_resume_elsewhere(lux, runners, a, b, run_id)


def test_claude_code_stop_is_sigint(lux, runners, hosts, fake_image):
    """Stopping Claude Code sends SIGINT (a clean turn end), not SIGTERM."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, adapter="claude-code", prompt="echo started\nsleep 60"))
    lux.wait_output(run_id, "started")
    lux.run("stop", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "stopped"
    assert run["placements"][0]["exitCode"] == 130, run["placements"][0]




def test_codex_adapter_with_app_server(lux, runners, hosts, fake_image):
    """The codex adapter over app-server: thread and turn, native mid-turn
    steering (turn/steer), interrupt (turn/interrupt), and thread/resume on
    another host."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, adapter="codex", prompt="write a.txt from-a\necho turn-one\nsleep 2\necho turn-one-done"))
    lux.wait_output(run_id, "turn-one")
    # Mid-turn: steered into the running turn (turn/steer), not queued
    # behind it as a second turn.
    lux.run("steer", run_id, "echo steered-in")
    out = lux.wait_output(run_id, "steered-in")
    assert out.index("turn-one-done") < out.index("steered-in")
    run = lux.wait_activity(run_id, "idle")
    turns = [r["event"]["data"]["turn"]["id"] for r in lux.records(run_id, "--events")
             if r.get("event", {}).get("type") == "codex.turn/completed"]
    assert turns == ["turn-1"], f"the steer started its own turn: {turns}"
    assert run["sessionId"].startswith("fake-")
    delivered = wait_until(lambda: events_of(lux, run_id, "input.delivered"), 20, 0.3, "steer not acknowledged")
    assert delivered

    # A new turn once idle.
    lux.run("steer", run_id, "echo turn-two")
    lux.wait_output(run_id, "turn-two")
    lux.wait_activity(run_id, "idle")

    interrupt_then_resume_elsewhere(lux, runners, a, b, run_id)


def test_streamed_replies_read_one_per_line(lux, runners, hosts, fake_image):
    """ACP agents stream replies in chunks without line breaks (lux-fake
    does, like OpenCode): each reply still reads as its own line."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo alpha-reply"))
    lux.wait_activity(run_id, "idle")
    lux.run("steer", run_id, "echo beta-reply")
    lux.wait_output(run_id, "beta-reply")
    lux.wait_activity(run_id, "idle")
    lines = [l.strip() for l in lux.logs(run_id).splitlines()]
    assert "alpha-reply" in lines and "beta-reply" in lines, lines


def test_codex_key_as_an_env_secret(lux, runners, hosts, fake_image):
    """Codex reads its key from ~/.codex/auth.json; given OPENAI_API_KEY as
    an ordinary secret, the adapter writes that file (on the secrets tmpfs,
    never snapshotted)."""
    runners.start(hosts[0])
    spec = fake_agent(fake_image, adapter="codex", prompt="cat-file /home/agent/.codex/auth.json",
                      secrets=[{"name": "OPENAI_API_KEY", "value": "sk-test-abcdef"}])
    run_id = lux.submit(spec)
    lux.wait_activity(run_id, "idle")
    out = lux.logs(run_id)
    # The file is there with the key (redacted in output, as every secret).
    assert '"auth_mode":"apikey"' in out, out
    assert "[REDACTED:OPENAI_API_KEY]" in out and "sk-test-abcdef" not in out, out
    # The file lux wrote is on the tmpfs; the home volume holds only a link.
    # (What the agent itself writes to its volumes is the agent's business:
    # lux-fake's transcript records the reply above, key and all.)
    host = hosts[0]
    link = host.exec("sh", "-c", "readlink $(podman volume inspect --format '{{.Mountpoint}}' "
                     f"lux-{run_id}-home)/.codex/auth.json").strip()
    assert link.startswith("/.lux/secrets/"), link
    lux.run("stop", run_id, "--wait")
    grep = host.exec("sh", "-c", "grep -rl sk-test-abcdef /var/lib/lux 2>/dev/null; true")
    assert grep.strip() == "", f"the key reached the runner's disk: {grep}"
