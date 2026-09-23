"""Coding agents through lux, one set of tests for every harness.

Each test takes a `harness` (see tests/harnesses.py) and runs once per
agent: against lux-fake speaking that agent's protocol (always), and
against the real CLI (with -m agents and its credentials). Behaviour that
legitimately differs between protocols is asserted from the harness's
capabilities, never its name.
"""

from __future__ import annotations

from conftest import harnesses
from env import wait_until


def test_starts_reports_a_session_and_waits_for_input(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("ready")))
    run = lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    assert run["state"] == "running"
    assert run["sessionId"], "the adapter reported no session id"
    assert "ready" in lux.logs(run_id).lower()
    assert "waiting for input" in lux.run("ls").stdout


def test_steering_is_delivered_and_acknowledged(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("first")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.say("second"), "--request-id", "req-1")
    wait_until(lambda: "second" in lux.logs(run_id).lower(), harness.timeout, 0.5, "steer never answered")
    acks = wait_until(lambda: lux.events(run_id, "input.delivered"), 20, 0.3, "no delivery ack")
    assert acks[0]["data"]["requestId"] == "req-1"


def test_replies_read_one_per_line(lux, runners, hosts, harness):
    """Agents stream replies in pieces; each still reads as its own line."""
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("alpha")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.say("beta"))
    wait_until(lambda: "beta" in lux.logs(run_id).lower(), harness.timeout, 0.5, "no second reply")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lines = [l.strip().strip(".").lower() for l in lux.logs(run_id).splitlines()]
    assert "alpha" in lines and "beta" in lines, lines


def test_interrupt_ends_the_turn_not_the_run(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("ready")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.long_turn())
    lux.wait_activity(run_id, "busy", timeout=harness.timeout)
    lux.run("interrupt", run_id)
    run = lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    assert run["state"] == "running"
    assert "never" not in lux.logs(run_id).lower().split()


def test_mid_turn_steering(lux, runners, hosts, harness):
    """Input sent during a turn is never lost. Where the protocol can, it
    joins the running turn (Codex); otherwise it runs after it."""
    if harness.real:
        import pytest
        pytest.skip("depends on scripted timing")
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec("echo turn-one\nsleep 2\necho turn-one-done"))
    lux.wait_output(run_id, "turn-one")
    lux.run("steer", run_id, "echo steered")
    out = lux.wait_output(run_id, "steered")
    assert out.index("turn-one-done") < out.index("steered")
    lux.wait_activity(run_id, "idle")
    turn_ends = [r for r in lux.records(run_id, "--events")
                 if r.get("event", {}).get("type") in ("codex.turn/completed", "claude.result", "acp.turn_end")]
    assert len(turn_ends) == (1 if harness.caps.steer_joins_turn else 2), turn_ends


def test_stop_resume_elsewhere_and_remember(lux, runners, hosts, harness):
    """The core story: write a file, be steered, stop, resume on another
    host, and answer from the restored conversation."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    spec = harness.spec(harness.write("/workspace/word.txt", "PINEAPPLE"))
    run_id = lux.submit(spec)
    session = lux.wait_activity(run_id, "idle", timeout=harness.timeout)["sessionId"]
    lux.run("steer", run_id, harness.say("steered"))
    wait_until(lambda: "steered" in lux.logs(run_id).lower(), harness.timeout, 0.5, "steer never answered")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)

    lux.run("stop", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "stopped"
    # A graceful stop: the adapter wound the agent down; it was not killed
    # after the grace period (137), nor, where the protocol wants SIGINT,
    # sent SIGTERM (143).
    code = run["placements"][0]["exitCode"]
    assert code != 137, "the agent had to be killed: its graceful stop did not work"
    if harness.caps.stop_is_sigint:
        assert code != 143, "stopped with SIGTERM; this agent needs SIGINT to end its turn cleanly"
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)

    since = lux.records(run_id)[-1]["cursor"]
    lux.run("resume", run_id, "--wait", "--input", harness.recall("word.txt"), *harness.resume_secrets(spec))
    wait_until(lambda: "PINEAPPLE" in lux.logs(run_id, "--since", since).upper(), harness.timeout * 1.5, 1,
               "resumed session did not remember")
    run = lux.get(run_id)
    assert [p["hostName"] for p in run["placements"]] == [a.name, b.name]
    assert run["sessionId"] == session, "the conversation restarted instead of continuing"
    out = lux.logs(run_id)
    for v in harness.secret_values():
        assert v not in out, "a credential leaked into output"
    lux.run("cancel", run_id)


@harnesses(lambda h: h.name == "codex")
def test_codex_key_as_an_env_secret(lux, runners, hosts, harness):
    """Codex reads its key from ~/.codex/auth.json; the adapter writes that
    file from an OPENAI_API_KEY secret, on the secrets tmpfs, so it never
    reaches disk. (Fake only: it reads the file back.)"""
    if harness.real:
        import pytest
        pytest.skip("the real variant covers this by authenticating at all")
    runners.start(hosts[0])
    spec = harness.spec("cat-file /home/agent/.codex/auth.json",
                        secrets=[{"name": "OPENAI_API_KEY", "value": "sk-test-abcdef"}])
    run_id = lux.submit(spec)
    lux.wait_activity(run_id, "idle")
    out = lux.logs(run_id)
    assert '"auth_mode":"apikey"' in out and "[REDACTED:OPENAI_API_KEY]" in out, out
    assert "sk-test-abcdef" not in out
    host = hosts[0]
    link = host.exec("sh", "-c", "readlink $(podman volume inspect --format '{{.Mountpoint}}' "
                     f"lux-{run_id}-home)/.codex/auth.json").strip()
    assert link.startswith("/.lux/secrets/"), link
    lux.run("stop", run_id, "--wait")
    assert host.exec("sh", "-c", "grep -rl sk-test-abcdef /var/lib/lux 2>/dev/null; true").strip() == ""
