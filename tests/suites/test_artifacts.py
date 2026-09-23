"""Step 12: artifacts. Files a Run produces (artifacts.paths, or published
into $LUX_ARTIFACTS) are collected on every exit, uploaded like snapshots,
listed and downloaded through lux — as the files the Run wrote."""

from __future__ import annotations

import hashlib

import pytest

from conftest import CLIError, generic
from env import ALPINE_IMAGE, wait_until

WORKSPACE = [{"name": "workspace", "path": "/workspace", "kind": "state"}]


def artifacts_run(lux, script: str, paths: list[str]) -> str:
    return lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, volumes=WORKSPACE, artifacts={"paths": paths}))


def wait_available(lux, run_id: str, n: int) -> list[dict]:
    return wait_until(lambda: (a := lux.json("artifacts", run_id)) and len(a) >= n and all(x["available"] for x in a) and a,
                      60, 0.5, "artifacts never became available")


def test_collected_listed_and_downloaded_as_written(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    script = ("mkdir -p /workspace/out/sub && echo report > /workspace/out/report.txt && "
              "head -c 200000 /dev/urandom > /workspace/out/sub/data.bin && "
              "echo '{\"ok\":true}' > /workspace/out/result.json && echo skip > /workspace/other.txt && "
              "echo published > $LUX_ARTIFACTS/note.md")
    run_id = artifacts_run(lux, script, ["/workspace/out/**"])
    lux.wait_state(run_id, "succeeded")
    arts = wait_available(lux, run_id, 4)
    paths = sorted(a["path"] for a in arts)
    assert paths == ["/.lux/artifacts/note.md", "/workspace/out/report.txt", "/workspace/out/result.json",
                     "/workspace/out/sub/data.bin"], paths
    types = {a["path"]: a["contentType"] for a in arts}
    assert types["/workspace/out/result.json"].startswith("application/json"), types

    out = lux.run("artifacts", run_id, "--download", str(tmp_path)).stdout
    assert "report.txt" in out, out
    base = tmp_path / "1"
    assert (base / "workspace/out/report.txt").read_text() == "report\n"
    assert (base / ".lux/artifacts/note.md").read_text() == "published\n"
    assert len((base / "workspace/out/sub/data.bin").read_bytes()) == 200000


def test_artifacts_match_what_the_workload_wrote(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    script = ("mkdir -p /workspace/out && head -c 65536 /dev/urandom > /workspace/out/r.bin && "
              "sha256sum /workspace/out/r.bin | cut -d' ' -f1")
    run_id = artifacts_run(lux, script, ["/workspace/out/*.bin"])
    lux.wait_state(run_id, "succeeded")
    want = lux.logs(run_id).strip()
    wait_available(lux, run_id, 1)
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    assert hashlib.sha256((tmp_path / "1/workspace/out/r.bin").read_bytes()).hexdigest() == want


def test_every_exit_collects(lux, runners, hosts):
    """A stop collects too, and a resumed placement's artifacts are listed
    by epoch; published ones are not collected twice."""
    runners.start(hosts[0])
    script = "mkdir -p /workspace/out; date +%s%N > /workspace/out/stamp; echo x > $LUX_ARTIFACTS/once.txt; sleep 300"
    run_id = artifacts_run(lux, script, ["/workspace/out/stamp"])
    wait_until(lambda: lux.run("exec", run_id, "-T", "--", "test", "-f", "/workspace/out/stamp", input="", check=False).returncode == 0,
               30, 0.5, "the workload never wrote its stamp")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id)
    lux.wait_state(run_id, "running")
    lux.run("cancel", run_id)
    lux.wait_state(run_id, "cancelled")
    arts = wait_available(lux, run_id, 3)
    by_epoch = sorted((a["epoch"], a["path"]) for a in arts)
    assert (1, "/workspace/out/stamp") in by_epoch and (2, "/workspace/out/stamp") in by_epoch, by_epoch
    # once.txt was published in both placements (the script runs again),
    # but each placement's is its own: collected then cleared.
    assert by_epoch.count((1, "/.lux/artifacts/once.txt")) == 1, by_epoch


def test_symlinks_are_not_followed(lux, runners, hosts):
    """A workload's symlink could point anywhere on the host: it is never
    collected (and cannot lead the collection out of the volume)."""
    host = runners.start(hosts[0])
    host.exec("sh", "-c", "echo host-secret > /root/host-secret.txt")
    script = ("mkdir -p /workspace/out && ln -s /root/host-secret.txt /workspace/out/leak.txt && "
              "ln -s / /workspace/out/rootdir && echo fine > /workspace/out/fine.txt && "
              "ln -s /root/host-secret.txt $LUX_ARTIFACTS/leak2.txt")
    run_id = artifacts_run(lux, script, ["/workspace/out/**"])
    lux.wait_state(run_id, "succeeded")
    arts = wait_available(lux, run_id, 1)
    assert [a["path"] for a in arts] == ["/workspace/out/fine.txt"], arts


def test_another_tenant_cannot_see_them(lux, tenant_factory, runners, hosts):
    runners.start(hosts[0])
    run_id = artifacts_run(lux, "mkdir -p /workspace/out && echo a > /workspace/out/a.txt", ["/workspace/out/*"])
    lux.wait_state(run_id, "succeeded")
    art = wait_available(lux, run_id, 1)[0]
    other = tenant_factory()
    with pytest.raises(CLIError):
        other.run("artifacts", run_id)
    assert other.api(f"/v1/artifacts/{art['id']}").status_code == 404


def test_collected_after_a_runner_restart(lux, runners, hosts):
    """A Run re-adopted after its runner restarts still has its spec: its
    artifacts are collected when it exits."""
    runners.start(hosts[0])
    script = "mkdir -p /workspace/out && echo late > /workspace/out/late.txt && sleep 5"
    run_id = artifacts_run(lux, script, ["/workspace/out/*"])
    lux.wait_state(run_id, "running")
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    lux.wait_state(run_id, "succeeded", timeout=60)
    assert [a["path"] for a in wait_available(lux, run_id, 1)] == ["/workspace/out/late.txt"]
