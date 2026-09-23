"""Real coding agents, driven through lux like any workload. Opt-in: these
make model calls, so each runs only when its credentials are set:

  Claude Code  LUX_TEST_ANTHROPIC_API_KEY [LUX_TEST_ANTHROPIC_BASE_URL]
  Codex        LUX_TEST_OPENAI_API_KEY    [LUX_TEST_OPENAI_BASE_URL] [LUX_TEST_CODEX_MODEL]
  OpenCode     LUX_TEST_OPENCODE_AUTH     (its auth.json), LUX_TEST_OPENCODE_CONFIG (its opencode.json),
               LUX_TEST_OPENCODE_MODEL    (provider/model)

Select the suite (`suites/test_agents_real.py`, or `-m agents`) so the
harness builds the agents' images. Every test tells the same story: do
something, be steered, stop, resume on another host, and remember.
Credentials go in as secrets, never as mounted config."""

from __future__ import annotations

import json
import os

import pytest

from conftest import AGENT_VOLUMES
from env import wait_until

pytestmark = pytest.mark.agents

WRITE = ("Create the file /workspace/word.txt containing exactly the word PINEAPPLE, "
         "then reply with just: done")
ASK = "Without using any tools: what word did you write to word.txt earlier? Reply with just the word."


def agent_image(env, name: str) -> str:
    img = env.extra.get("images", {}).get(name)
    if not img:
        pytest.skip(f"no {name} image: set its credentials (see this module's docstring) and select this suite")
    return img


def agent_spec(image: str, adapter: str, command: list[str], secrets: list[dict], env: dict | None = None) -> dict:
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": adapter, "prompt": WRITE, "workdir": "/workspace", "command": command},
        "volumes": [dict(v) for v in AGENT_VOLUMES],
        "secrets": secrets,
        "network": {"unrestricted": True},
    }
    if env:
        spec["env"] = env
    return spec


def remember_across_hosts(lux, runners, hosts, spec: dict, key_values: list[str]):
    """The shared story: write a word, get steered, stop, resume on the
    other host, and answer from the restored conversation. The key values
    must never appear in the output."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(spec)
    assert lux.wait_activity(run_id, "idle", timeout=240)["sessionId"]
    lux.run("steer", run_id, "Reply with just the word: steered")
    wait_until(lambda: "steered" in lux.logs(run_id).lower(), 240, 2, "steer never answered")
    lux.wait_activity(run_id, "idle", timeout=240)

    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    since = lux.records(run_id)[-1]["cursor"]
    resume = ["resume", run_id, "--wait", "--input", ASK]
    for sec in spec["secrets"]:
        resume += ["--secret", f"{sec['name']}={sec['value']}"]
    lux.run(*resume)
    wait_until(lambda: "PINEAPPLE" in lux.logs(run_id, "--since", since).upper(), 300, 2,
               "resumed session did not remember")
    out = lux.logs(run_id)
    print("\n--- transcript ---\n" + out)
    run = lux.get(run_id)
    assert [p["hostName"] for p in run["placements"]] == [a.name, b.name]
    for v in key_values:
        assert v not in out, "a credential leaked into output"
    lux.run("cancel", run_id)


# ---- Claude Code --------------------------------------------------------------


def test_claude_code_runs_steers_and_resumes_elsewhere(env, lux, runners, hosts):
    image = agent_image(env, "claude")
    key = os.environ["LUX_TEST_ANTHROPIC_API_KEY"]
    base = os.environ.get("LUX_TEST_ANTHROPIC_BASE_URL")
    spec = agent_spec(image, "claude-code",
                      ["claude", "--model", "haiku", "--permission-mode", "bypassPermissions"],
                      [{"name": "ANTHROPIC_API_KEY", "value": key}],
                      {"ANTHROPIC_BASE_URL": base} if base else None)
    remember_across_hosts(lux, runners, hosts, spec, [key])


# ---- Codex --------------------------------------------------------------------


def test_codex_runs_steers_and_resumes_elsewhere(env, lux, runners, hosts):
    image = agent_image(env, "codex")
    key = os.environ["LUX_TEST_OPENAI_API_KEY"]
    cmd = ["codex", "-c", "approval_policy=never", "-c", "sandbox_mode=danger-full-access"]
    if base := os.environ.get("LUX_TEST_OPENAI_BASE_URL"):
        cmd += ["-c", f'openai_base_url="{base}"']
    if model := os.environ.get("LUX_TEST_CODEX_MODEL"):
        cmd += ["-c", f"model={model}"]
    # An ordinary env secret: the codex adapter writes the auth.json Codex reads.
    spec = agent_spec(image, "codex", cmd, [{"name": "OPENAI_API_KEY", "value": key}])
    remember_across_hosts(lux, runners, hosts, spec, [key])


# ---- OpenCode -----------------------------------------------------------------


def test_opencode_runs_steers_and_resumes_elsewhere(env, lux, runners, hosts):
    image = agent_image(env, "opencode")
    auth = os.environ["LUX_TEST_OPENCODE_AUTH"]
    config = json.loads(os.environ["LUX_TEST_OPENCODE_CONFIG"])
    config["model"] = os.environ["LUX_TEST_OPENCODE_MODEL"]
    # OpenCode reads providers from opencode.json and keys from auth.json:
    # real user config, so both go in as file secrets (tmpfs, never snapshotted).
    spec = agent_spec(image, "opencode", ["opencode", "acp"], [
        {"name": "opencode_auth", "value": auth, "as": "file", "path": "/home/agent/.local/share/opencode/auth.json"},
        {"name": "opencode_config", "value": json.dumps(config), "as": "file",
         "path": "/home/agent/.config/opencode/opencode.json"},
    ])
    keys = [p["key"] for p in json.loads(auth).values() if isinstance(p, dict) and p.get("key")]
    remember_across_hosts(lux, runners, hosts, spec, keys)
