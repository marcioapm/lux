"""Files: local files lux writes into the container before init, on every
start, as the workload user. The CLI reads them (a skills folder, --copy)
and luxd keeps their bytes beside the spec: never in it, in the
environment or in a placement's acked message. A resume can replace them;
a link the workload plants is replaced, never written through."""

from __future__ import annotations

import base64
import json

import psycopg

from conftest import CLIError, generic
from env import ALPINE_IMAGE

WORKSPACE = [{"name": "home", "path": "/home/agent", "kind": "state"}]
MARKER = "lux-files-marker-3b9d1c"


def skills(tmp_path, image: str, version: str):
    """A skills folder and a spec that ships it to ~/.claude/skills; init
    lists what it finds there, before the workload starts. The image's
    agent user is uid 1000."""
    root = tmp_path / version
    (root / "skills" / "review").mkdir(parents=True)
    (root / "skills" / "review" / "SKILL.md").write_text(f"# review {version} {MARKER}\n")
    (root / "skills" / "review" / "SKILL.md").chmod(0o644)
    run = root / "skills" / "review" / "run.sh"
    run.write_text("#!/bin/sh\necho ran\n")
    run.chmod(0o755)
    spec = generic(image, "sh", "-c", "echo up-$LUX_EPOCH; sleep 300", volumes=WORKSPACE)
    spec["workload"]["user"] = "agent"
    spec["init"] = {"script": "ls -ln ~/.claude/skills/review | awk 'NR>1 {print \"init-saw\", $1, $3, $9}'; "
                              "cat ~/.claude/skills/review/SKILL.md"}
    spec["files"] = [{"path": "~/.claude/skills", "source": "skills"}]
    path = root / "spec.yaml"
    path.write_text(json.dumps(spec))
    return path


def test_files_are_in_place_before_init_as_the_workload_user(env, lux, runners, hosts, fake_image, tmp_path):
    runners.start(hosts[0])
    spec = skills(tmp_path, fake_image, "v1")
    run_id = lux.run("run", "-f", str(spec), "--copy", f"{spec.parent / 'skills' / 'review' / 'SKILL.md'}:/tmp/copied.md").stdout.strip()
    out = lux.wait_output(run_id, "up-1", timeout=120)
    # init saw both files, owned by uid 1000 (agent), with their modes.
    assert "init-saw -rw-r--r-- 1000 SKILL.md" in out, out
    assert "init-saw -rwxr-xr-x 1000 run.sh" in out, out
    assert f"# review v1 {MARKER}" in out, out
    files_ev = [r["event"]["data"] for r in lux.records(run_id, "--events") if r.get("event", {}).get("type") == "lux.files"]
    assert files_ev and files_ev[0]["count"] == 3, files_ev
    copied = lux.run("exec", run_id, "-T", "--", "sh", "-c", "stat -c '%u %a' /tmp/copied.md ~/.claude/skills; env", input="").stdout
    assert copied.splitlines()[0] == "1000 644", copied
    assert copied.splitlines()[1] == "1000 755", copied
    # Never in the environment.
    assert MARKER not in copied, copied

    # Never in the spec, and gone from the assignment once acked.
    with psycopg.connect(env.owner_dsn) as conn:
        stored = conn.execute("SELECT spec::text FROM runs WHERE id = %s", (run_id,)).fetchone()[0]
        payloads = [r[0] for r in conn.execute("SELECT payload::text FROM host_messages WHERE run_id = %s AND acked_at IS NOT NULL", (run_id,))]
    encoded = base64.b64encode(f"# review v1 {MARKER}\n".encode()).decode()
    assert encoded not in stored and '"sha256"' in stored, stored
    assert payloads and not any(encoded in p for p in payloads), payloads
    shown = lux.get(run_id)["spec"]["files"]
    assert {f["path"] for f in shown} >= {"~/.claude/skills/review/SKILL.md", "/tmp/copied.md"}, shown
    assert all("data" not in f for f in shown), shown
    lux.run("terminate", run_id)


def test_a_resume_writes_them_again_and_can_replace_them(lux, runners, hosts, fake_image, tmp_path):
    runners.start(hosts[0])
    spec = skills(tmp_path, fake_image, "v1")
    run_id = lux.run("run", "-f", str(spec)).stdout.strip()
    lux.wait_output(run_id, "up-1", timeout=120)
    # The workload edits one and plants a link where the other goes: the
    # link's target is the workload's own file, which must stay as it is.
    lux.run("exec", run_id, "-T", "--", "sh", "-c",
            "echo edited > ~/.claude/skills/review/SKILL.md; echo mine > ~/target; "
            "rm ~/.claude/skills/review/run.sh; ln -s ~/target ~/.claude/skills/review/run.sh", input="")
    lux.run("stop", run_id, "--wait")

    # A plain resume writes the Run's files again, replacing the link.
    lux.run("resume", run_id)
    lux.wait_output(run_id, "up-2", timeout=120)
    check = lux.run("exec", run_id, "-T", "--", "sh", "-c",
                    "cat ~/.claude/skills/review/SKILL.md ~/target; [ -L ~/.claude/skills/review/run.sh ] && echo still-a-link; true", input="").stdout
    assert f"# review v1 {MARKER}" in check and "mine" in check and "still-a-link" not in check, check
    lux.run("stop", run_id, "--wait")

    # An edited folder reaches the resumed Run.
    v2 = skills(tmp_path, fake_image, "v2")
    lux.run("resume", run_id, "--files-from", str(v2))
    lux.wait_output(run_id, "up-3", timeout=120)
    assert f"# review v2 {MARKER}" in lux.logs(run_id), lux.logs(run_id)
    assert any(e["data"].get("files", {}).get("count") == 2 for e in lux.events(run_id, "resume.requested"))
    lux.run("terminate", run_id)


def test_a_link_on_the_way_fails_the_start(lux, runners, hosts, fake_image, tmp_path):
    """The workload made ~/.claude a link to somewhere else: lux follows no
    link, so the next start fails, saying which."""
    runners.start(hosts[0])
    spec = skills(tmp_path, fake_image, "v1")
    run_id = lux.run("run", "-f", str(spec)).stdout.strip()
    lux.wait_output(run_id, "up-1", timeout=120)
    lux.run("exec", run_id, "-T", "--", "sh", "-c", "rm -rf ~/.claude && mkdir ~/elsewhere && ln -s ~/elsewhere ~/.claude", input="")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id)
    run = lux.wait_state(run_id, "failed", "running", timeout=120)
    assert run["state"] == "failed", run
    why = lux.logs(run_id, "--events") + run.get("stateReason", "")
    assert "/.claude is a symbolic link" in why, why


def test_dry_run_checks_files(lux, tmp_path):
    (tmp_path / "big").write_bytes(b"x" * (3 << 20))
    try:
        lux.run("run", "--dry-run", "--image", ALPINE_IMAGE, "--copy", f"{tmp_path / 'big'}:/a",
                "--copy", f"{tmp_path / 'big'}:/b", "--", "true")
        raise AssertionError("4 MiB passed")
    except CLIError as e:
        assert "in total" in e.stderr, e.stderr
    bad = tmp_path / "bad.yaml"
    bad.write_text(json.dumps({**generic(ALPINE_IMAGE, "true"), "files": [{"path": "/.lux/run/x", "content": "x"}]}))
    try:
        lux.run("run", "--dry-run", "-f", str(bad))
        raise AssertionError("a reserved path passed")
    except CLIError as e:
        assert "reserved" in e.stderr, e.stderr
