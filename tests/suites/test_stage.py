"""A Run's stage: luxd knows which phase of its start a Run is in, and since
when, as the runner reaches each one, and announces each change as a
run.stage event."""

from __future__ import annotations

import json
import threading
import time
from datetime import datetime

import requests

from conftest import generic
from env import ALPINE_IMAGE, wait_until

STAGES = ["waiting", "image", "volumes", "repositories", "container", "running", "succeeded"]


def at(ts: str) -> datetime:
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


class Feed:
    """GET /v1/events, followed in a thread: every run.stage event, in the
    order the stream delivers them."""

    def __init__(self, lux):
        self.stages: list[dict] = []
        self.ready = threading.Event()
        self._r = requests.get(f"{lux.env.luxd_url}/v1/events", headers={"Authorization": f"Bearer {lux.api_key}"},
                               stream=True, timeout=(10, 120))
        self._t = threading.Thread(target=self._read, daemon=True)
        self._t.start()

    def _read(self):
        self.ready.set()
        try:
            for line in self._r.iter_lines(decode_unicode=True):
                if line and line.startswith("data: "):
                    e = json.loads(line[6:])
                    if e.get("type") == "run.stage":
                        self.stages.append(e)
        except (requests.RequestException, AttributeError):
            pass  # closed by close(): a closed response reads from no socket

    def of(self, run_id: str) -> list[dict]:
        return [e["data"] for e in list(self.stages) if e.get("runId") == run_id]

    def close(self):
        self._r.close()
        self._t.join(5)


def test_a_starting_run_reports_its_stage(lux, runners, hosts, git_server):
    """A Run whose image takes seconds to build, with a repository to clone:
    while it builds, GET says stage image; the stage then moves forward
    through the start, stageSince never going back, and the event stream
    carries each change once, in order."""
    git_server.create("stage", {"README.md": "stage\n"})
    runners.start(hosts[0])
    feed = Feed(lux)
    feed.ready.wait(10)
    time.sleep(0.5)  # the stream is open before the Run exists
    try:
        cf = f"FROM {ALPINE_IMAGE}\nRUN sleep 6 && echo {time.time_ns()} > /built\n"
        spec = generic(ALPINE_IMAGE, "sleep", "3")
        spec["image"] = {"build": {"containerfile": cf}}
        spec["volumes"] = [{"name": "workspace", "path": "/workspace", "kind": "state"}]
        spec["git"] = {"repositories": [{"name": "stage", "url": git_server.url("stage"), "ref": "main", "credential": "GIT_TOKEN"}]}
        spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
        run_id = lux.submit(spec)

        seen: list[tuple[str, datetime, str]] = []  # (stage, since, state) as GET returned them
        deadline = time.time() + 120
        while time.time() < deadline:
            r = lux.api(f"/v1/runs/{run_id}").json()
            entry = (r["stage"], at(r["stageSince"]), r["state"])
            if not seen or seen[-1][:2] != entry[:2]:
                seen.append(entry)
            if r["stage"] == "succeeded":
                break
            time.sleep(0.2)
        assert seen and seen[-1][0] == "succeeded", seen
        print("stages read:", [(s, since.isoformat(), state) for s, since, state in seen])

        # Read while it started: the build, as stage image, while the Run was
        # starting (not only once it ran).
        assert any(s == "image" and state in ("scheduled", "starting") for s, _, state in seen), seen
        order = [STAGES.index(s) for s, _, _ in seen]
        assert order == sorted(order), f"stage went back: {seen}"
        sinces = [since for _, since, _ in seen]
        assert sinces == sorted(sinces), f"stageSince went back: {seen}"

        # The placement's marks as the API reports them, reposReady included.
        pl = lux.get(run_id)["placements"][0]
        for k in ("acceptedAt", "imageReadyAt", "volumesRestoredAt", "reposReadyAt", "containerStartedAt"):
            assert pl.get(k), (k, pl)
        assert at(pl["imageReadyAt"]) <= at(pl["volumesRestoredAt"]) <= at(pl["reposReadyAt"]) <= at(pl["containerStartedAt"]), pl
        # reposReady follows the clone: the runner reports git.clone before it.
        clone = lux.events(run_id, "git.clone")
        assert clone and clone[0]["data"]["status"] == "cloned", clone

        # The events: one per change, in order, through the build and to the end.
        events = wait_until(lambda: (lambda e: e if e and e[-1]["stage"] == "succeeded" else None)(feed.of(run_id)),
                            20, 0.2, "the feed never announced succeeded")
        stages = [e["stage"] for e in events]
        idx = [STAGES.index(s) for s in stages]
        assert idx == sorted(set(idx)), f"run.stage events out of order or repeated: {stages}"
        for want in ("waiting", "image", "running", "succeeded"):
            assert want in stages, stages
        # luxd learned a phase past the image while the Run was starting,
        # not only from the running report's marks.
        assert {"volumes", "repositories", "container"} & set(stages), stages
        ev_since = [at(e["since"]) for e in events]
        assert ev_since == sorted(ev_since), events
        assert all(e["epoch"] == 1 for e in events[1:]), events
        # The image stage the event announced is the one GET showed.
        image = next(e for e in events if e["stage"] == "image")
        assert any(s == "image" and since == at(image["since"]) for s, since, _ in seen), (image, seen)
        assert at(image["since"]) == at(pl["acceptedAt"]), (image, pl)
        # The Run's own event list has the same, once each.
        assert [e["data"]["stage"] for e in lux.events(run_id, "run.stage")] == stages
    finally:
        feed.close()
