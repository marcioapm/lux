"""Step 2: output. Live from the host, relayed by luxd; after exit from S3;
one cursor across placements."""

from __future__ import annotations

import json
import threading
import time

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_logs_follow_streams_live(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in 1 2 3 4 5; do echo line-$i; sleep 1; done"))
    p = lux.popen("logs", run_id, "-f")
    first_at = None
    lines = []
    start = time.time()
    for line in p.stdout:
        lines.append(line.strip())
        if first_at is None:
            first_at = time.time() - start
    p.wait(timeout=30)
    assert lines == [f"line-{i}" for i in range(1, 6)], lines
    # Streamed, not delivered at the end: the first line arrived well before
    # the Run's five seconds were up.
    assert first_at is not None and first_at < 4, first_at


def test_output_after_exit_comes_from_s3(env, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo from-the-container; echo to-stderr >&2"))
    assert lux.wait_state(run_id, "succeeded")["state"] == "succeeded"
    # Wait for the upload, then take the host away: output must still be there.
    lux.wait_placement_uploaded(run_id)
    runners.stop(hosts[0])
    recs = lux.records(run_id)
    assert [r["data"] for r in recs if r["ch"] == "stdout"] == ["from-the-container\n"]
    assert [r["data"] for r in recs if r["ch"] == "stderr"] == ["to-stderr\n"]
    keys = [o["Key"] for o in env.s3().list_objects_v2(Bucket=env.bucket).get("Contents", [])]
    assert any(run_id in k for k in keys)


def test_cursor_resumes_where_it_left_off(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in $(seq 1 20); do echo n$i; done"))
    lux.wait_state(run_id, "succeeded")
    recs = lux.records(run_id)
    assert len(recs) >= 1
    mid = recs[len(recs) // 2]
    rest = lux.records(run_id, "--since", mid["cursor"])
    assert rest == recs[len(recs) // 2 + 1:]
    assert "".join(r["data"] for r in recs).split() == [f"n{i}" for i in range(1, 21)]


def test_events_and_lifecycle_in_the_stream(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "true"))
    lux.wait_state(run_id, "succeeded")
    out = lux.run("logs", run_id, "--events", "-o", "json").stdout
    items = [json.loads(l) for l in out.splitlines() if l.strip()]
    events = [i["event"]["type"] for i in items if i.get("ch") == "event"]
    assert "lux.workload" in events
    states = [i["lux"]["data"].get("state") for i in items if "lux" in i and i["lux"]["type"] == "state"]
    assert states[-1] == "succeeded", states


def test_follow_across_a_restart_of_the_runner(lux, runners, hosts):
    """The runner restarts mid-Run: the container keeps going (Podman is
    daemonless), the new runner re-adopts it, and the output stream picks
    up where it was."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in $(seq 1 12); do echo tick-$i; sleep 1; done"))
    lux.wait_output(run_id, "tick-2")
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=60)
    assert run["state"] == "succeeded", run
    assert len(run["placements"]) == 1, "re-adopted, not rescheduled"
    out = lux.logs(run_id)
    assert out.split() == [f"tick-{i}" for i in range(1, 13)], out


def test_follow_across_same_host_resumes(lux, runners, hosts):
    """A follower that reconnects from its cursor whenever its stream ends
    (at each stop, as `lux logs -f` does) gets every resumed placement's
    output while it runs, including when it reaches a new placement before
    the runner has taken it up. The runner holds each assignment for 3s, so
    the follower always finds the new placement still assigned."""
    runners.start(hosts[0], environ={"LUX_TEST_ASSIGN_DELAY": "3s"})
    spec = generic(ALPINE_IMAGE, "sh", "-c", 'i=0; while :; do i=$((i+1)); echo "e$LUX_EPOCH-$i"; sleep 0.3; done',
                   volumes=[{"name": "data", "path": "/data", "kind": "state"}])
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "e1-2")
    recs: list[dict] = []
    stop = threading.Event()

    def follow():
        cur = ""
        while not stop.is_set():
            p = lux.popen("logs", run_id, "-f", "-o", "json", *(["--since", cur] if cur else []))
            for line in p.stdout:
                if line.strip():
                    r = json.loads(line)
                    recs.append(r)
                    cur = r["cursor"]
                if stop.is_set():
                    p.kill()
                    break
            p.wait()
            time.sleep(0.2)

    follower = threading.Thread(target=follow, daemon=True)
    follower.start()
    try:
        epochs = lambda: {r["epoch"] for r in recs if r["ch"] == "stdout"}
        wait_until(lambda: 1 in epochs(), 60, 0.3, "the first placement's output never reached the follower")
        for epoch in range(2, 5):
            lux.run("stop", run_id, "--wait")
            lux.run("resume", run_id, "--wait")
            wait_until(lambda: epoch in epochs(), 30, 0.3,
                       f"placement {epoch}'s output never reached the follower (it has epochs {sorted(epochs())})")
        run = lux.get(run_id)
        assert {p["hostName"] for p in run["placements"]} == {hosts[0].name}, run["placements"]
        for r in recs:
            if r["ch"] == "stdout":
                assert r["data"].startswith(f"e{r['epoch']}-"), r
    finally:
        stop.set()
        lux.run("cancel", run_id, "--wait", check=False)
        follower.join(timeout=30)
