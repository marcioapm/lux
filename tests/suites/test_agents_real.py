"""Real coding agents, driven through lux like any workload. Opt-in: these
make model calls, so each runs only when its credentials are set:

  Claude Code  LUX_TEST_ANTHROPIC_API_KEY [LUX_TEST_ANTHROPIC_BASE_URL]
  Codex        LUX_TEST_OPENAI_API_KEY    [LUX_TEST_OPENAI_BASE_URL] [LUX_TEST_CODEX_MODEL]
  OpenCode     LUX_TEST_OPENCODE_AUTH     (its auth.json), LUX_TEST_OPENCODE_CONFIG (its opencode.json),
               LUX_TEST_OPENCODE_MODEL    (provider/model)

Every test is the same story: do something, be steered, stop, resume on
another host, and remember. Credentials go in as secrets, never as
mounted config."""

from __future__ import annotations

import json
import os

import pytest

from env import wait_until

pytestmark = pytest.mark.agents

KEY = os.environ.get("LUX_TEST_ANTHROPIC_API_KEY", "")
BASE_URL = os.environ.get("LUX_TEST_ANTHROPIC_BASE_URL", "")


def agent_image(env, name: str, why: str) -> str:
    img = env.extra.get("images", {}).get(name)
    if not img:
        pytest.skip(why)
    return img


@pytest.fixture
def claude_image(env):
    return agent_image(env, "claude", "set LUX_TEST_ANTHROPIC_API_KEY (and have claude installed)")


@pytest.fixture
def codex_image(env):
    return agent_image(env, "codex", "set LUX_TEST_OPENAI_API_KEY (and have codex installed)")


@pytest.fixture
def opencode_image(env):
    return agent_image(env, "opencode", "set LUX_TEST_OPENCODE_AUTH/CONFIG/MODEL (and have opencode installed)")


def agent_home_volumes() -> list[dict]:
    return [
        {"name": "workspace", "path": "/workspace", "kind": "state"},
        {"name": "home", "path": "/home/agent", "kind": "state"},
    ]


def remember_across_hosts(lux, runners, hosts, spec: dict, resume_secrets: list[str], ask: str):
    """The shared story: write a word, get steered, stop, resume on the
    other host, and answer from the restored conversation."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(spec)
    run = lux.wait_activity(run_id, "idle", timeout=240)
    assert run["sessionId"], run
    lux.run("steer", run_id, "Reply with just the word: steered")
    wait_until(lambda: "steered" in lux.logs(run_id).lower(), 240, 2, "steer never answered")
    lux.wait_activity(run_id, "idle", timeout=240)

    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    before = lux.logs(run_id)
    args = ["resume", run_id, "--wait", "--input", ask]
    for s in resume_secrets:
        args += ["--secret", s]
    lux.run(*args)
    wait_until(lambda: "PINEAPPLE" in lux.logs(run_id)[len(before):].upper(), 300, 2,
               "resumed session did not remember")
    out = lux.logs(run_id)
    print("\n--- transcript ---\n" + out)
    run = lux.get(run_id)
    assert [p["hostName"] for p in run["placements"]] == [a.name, b.name]
    assert run["sessionId"]
    lux.run("cancel", run_id)
    return out


WRITE = ("Create the file /workspace/word.txt containing exactly the word PINEAPPLE, "
         "then reply with just: done")
ASK = "Without using any tools: what word did you write to word.txt earlier? Reply with just the word."


def claude_spec(image: str, prompt: str) -> dict:
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": "claude-code", "prompt": prompt, "workdir": "/workspace",
                     "command": ["claude", "--model", "haiku", "--permission-mode", "bypassPermissions"]},
        "volumes": agent_home_volumes(),
        "secrets": [{"name": "ANTHROPIC_API_KEY", "value": KEY}],
        "network": {"unrestricted": True},
    }
    if BASE_URL:
        spec["env"] = {"ANTHROPIC_BASE_URL": BASE_URL}
    return spec


def test_claude_code_runs_steers_and_resumes_elsewhere(lux, runners, hosts, claude_image):
    out = remember_across_hosts(lux, runners, hosts, claude_spec(claude_image, WRITE),
                                [f"ANTHROPIC_API_KEY={KEY}"], ASK)
    assert KEY not in out, "the API key leaked into output"


# ---- Codex ------------------------------------------------------------------

OPENAI_KEY = os.environ.get("LUX_TEST_OPENAI_API_KEY", "")
OPENAI_BASE_URL = os.environ.get("LUX_TEST_OPENAI_BASE_URL", "")
CODEX_MODEL = os.environ.get("LUX_TEST_CODEX_MODEL", "")


def codex_auth() -> str:
    """Codex reads its key from ~/.codex/auth.json (what `codex login
    --with-api-key` writes), not from the environment."""
    return json.dumps({"auth_mode": "apikey", "OPENAI_API_KEY": OPENAI_KEY})


def codex_spec(image: str, prompt: str) -> dict:
    cmd = ["codex", "-c", "approval_policy=never", "-c", "sandbox_mode=danger-full-access"]
    if OPENAI_BASE_URL:
        cmd += ["-c", f'openai_base_url="{OPENAI_BASE_URL}"']
    if CODEX_MODEL:
        cmd += ["-c", f"model={CODEX_MODEL}"]
    return {
        "image": {"ref": image},
        "workload": {"adapter": "codex", "prompt": prompt, "workdir": "/workspace", "command": cmd},
        "volumes": agent_home_volumes(),
        "secrets": [{"name": "codex_auth", "value": codex_auth(), "as": "file",
                     "path": "/home/agent/.codex/auth.json"}],
        "network": {"unrestricted": True},
    }


def test_codex_runs_steers_and_resumes_elsewhere(lux, runners, hosts, codex_image):
    out = remember_across_hosts(lux, runners, hosts, codex_spec(codex_image, WRITE),
                                [f"codex_auth={codex_auth()}"], ASK)
    assert OPENAI_KEY not in out


# ---- OpenCode ---------------------------------------------------------------

OPENCODE_AUTH = os.environ.get("LUX_TEST_OPENCODE_AUTH", "")
OPENCODE_CONFIG = os.environ.get("LUX_TEST_OPENCODE_CONFIG", "")
OPENCODE_MODEL = os.environ.get("LUX_TEST_OPENCODE_MODEL", "")


def opencode_config() -> str:
    """The developer's opencode.json with the test model as the default."""
    import json
    cfg = json.loads(OPENCODE_CONFIG)
    cfg["model"] = OPENCODE_MODEL
    return json.dumps(cfg)


def opencode_spec(image: str, prompt: str) -> dict:
    # OpenCode reads its providers from opencode.json and its keys from
    # auth.json; both go in as file secrets (tmpfs, never snapshotted).
    return {
        "image": {"ref": image},
        "workload": {"adapter": "opencode", "prompt": prompt, "workdir": "/workspace",
                     "command": ["opencode", "acp"]},
        "volumes": agent_home_volumes(),
        "secrets": [
            {"name": "opencode_auth", "value": OPENCODE_AUTH, "as": "file",
             "path": "/home/agent/.local/share/opencode/auth.json"},
            {"name": "opencode_config", "value": opencode_config(), "as": "file",
             "path": "/home/agent/.config/opencode/opencode.json"},
        ],
        "network": {"unrestricted": True},
    }


def test_opencode_runs_steers_and_resumes_elsewhere(lux, runners, hosts, opencode_image):
    if not (OPENCODE_CONFIG and OPENCODE_MODEL):
        pytest.skip("set LUX_TEST_OPENCODE_CONFIG and LUX_TEST_OPENCODE_MODEL too")
    out = remember_across_hosts(lux, runners, hosts, opencode_spec(opencode_image, WRITE),
                                [f"opencode_auth={OPENCODE_AUTH}", f"opencode_config={opencode_config()}"], ASK)
    assert OPENCODE_AUTH not in out

