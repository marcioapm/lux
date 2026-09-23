"""Real coding agents, driven through lux like any workload. Opt-in: these
make model calls, so they run only when LUX_TEST_ANTHROPIC_API_KEY is set
(and LUX_TEST_ANTHROPIC_BASE_URL, for a proxy)."""

from __future__ import annotations

import os

import pytest

from env import wait_until

pytestmark = pytest.mark.agents

KEY = os.environ.get("LUX_TEST_ANTHROPIC_API_KEY", "")
BASE_URL = os.environ.get("LUX_TEST_ANTHROPIC_BASE_URL", "")


@pytest.fixture
def claude_image(env):
    img = env.extra.get("images", {}).get("claude")
    if not KEY or not img:
        pytest.skip("set LUX_TEST_ANTHROPIC_API_KEY (and have claude installed) to run real-agent tests")
    return img


def claude_spec(image: str, prompt: str) -> dict:
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": "claude-code", "prompt": prompt, "workdir": "/workspace",
                     "command": ["claude", "--model", "haiku", "--permission-mode", "bypassPermissions"]},
        "volumes": [
            {"name": "workspace", "path": "/workspace", "kind": "state"},
            {"name": "home", "path": "/home/agent", "kind": "state"},
        ],
        "secrets": [{"name": "ANTHROPIC_API_KEY", "value": KEY}],
        "network": {"unrestricted": True},
    }
    if BASE_URL:
        spec["env"] = {"ANTHROPIC_BASE_URL": BASE_URL}
    return spec


def test_claude_code_runs_steers_and_resumes_elsewhere(lux, runners, hosts, claude_image):
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(claude_spec(claude_image,
        "Create the file /workspace/word.txt containing exactly the word PINEAPPLE. Then reply with just: done"))
    run = lux.wait_activity(run_id, "idle", timeout=180)
    assert run["sessionId"]
    lux.run("steer", run_id, "Reply with just the word: steered")
    wait_until(lambda: "steered" in lux.logs(run_id).lower(), 180, 2, "steer never answered")
    lux.wait_activity(run_id, "idle", timeout=180)

    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    before = lux.logs(run_id)
    lux.run("resume", run_id, "--wait", "--secret", f"ANTHROPIC_API_KEY={KEY}",
            "--input", "Without using any tools: what word did you write to word.txt earlier? Reply with just the word.")
    # The answer must come from the resumed conversation, after the resume.
    wait_until(lambda: "PINEAPPLE" in lux.logs(run_id)[len(before):].upper(), 240, 2,
               "resumed session did not remember")
    out = lux.logs(run_id)
    print("\n--- transcript ---\n" + out)
    assert KEY not in out, "the API key leaked into output"
    run = lux.get(run_id)
    assert [p["hostName"] for p in run["placements"]] == [a.name, b.name]
    lux.run("cancel", run_id)

