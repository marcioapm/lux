"""The workload runs as its user, with that user's supplementary groups
from the image's /etc/group, as `podman run` gives them: the workload
itself, an exec into it (with and without a terminal) and its beforeStop
hook, for the image's USER and for a user the spec names. uid, primary gid
and groups are compared separately."""

from __future__ import annotations

from conftest import generic
from env import ALPINE_IMAGE

# A CI-runner-like image: its user is in two extra groups.
CONTAINERFILE = f"""FROM {ALPINE_IMAGE}
RUN addgroup -g 4001 extra1 && addgroup -g 4002 extra2 && adduser -D -u 1000 runner \\
 && addgroup runner extra1 && addgroup runner extra2 && adduser -D -u 1001 run
USER runner
"""

# One line: "ids=<uid>|<gid>|<groups>".
IDS = 'echo "ids=$(id -u)|$(id -g)|$(id -G)"'
PUBLISH = "/.lux/bin/lux-shim publish"


def spec(*cmd: str, **workload) -> dict:
    s = generic(ALPINE_IMAGE, *cmd)
    s["image"] = {"build": {"containerfile": CONTAINERFILE}}
    s["workload"].update(workload)
    return s


def ids_line(text: str) -> tuple[str, str, str]:
    line = next(l for l in reversed(text.replace("\r", "").splitlines()) if l.startswith("ids="))
    uid, gid, groups = line.removeprefix("ids=").split("|")
    return uid, gid, groups


def podman_ids(host, image: str, *user: str) -> tuple[str, str, str]:
    args = ["--user", user[0]] if user else []
    return ids_line(host.podman("run", "--rm", *args, image, "sh", "-c", IDS))


def test_workload_gets_the_images_groups(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    run_id = lux.submit(spec("sh", "-c", f"{IDS}; sleep 300",
                             beforeStop={"command": ["sh", "-c", f"{IDS} > /tmp/hook-ids; {PUBLISH} /tmp/hook-ids"], "timeout": "10s"}))
    lux.wait_output(run_id, "ids=")
    image = lux.get(run_id)["image"]["imageId"]
    want = podman_ids(hosts[0], image)
    assert want == ("1000", "1000", "1000 4001 4002"), want
    assert ids_line(lux.logs(run_id)) == want
    # An exec into the Run runs as the same user, with the same groups,
    # without a terminal and with one.
    assert ids_line(lux.run("exec", run_id, "-T", "--", "sh", "-c", IDS, input="").stdout) == want
    assert ids_line(lux.run("exec", run_id, "-t", "--", "sh", "-c", IDS, input="").stdout) == want
    # So does the beforeStop hook.
    lux.run("stop", run_id, "--wait")
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    assert ids_line((tmp_path / "1/.lux/artifacts/hook-ids").read_text()) == want

    # A user the spec names: by name, by uid in its forms, one in no extra
    # group; uid:gid gets that group only. "runner:" gets runner's groups,
    # as runc gives them (podman with crun gives its primary group only).
    for user, podman_user in [("runner", "runner"), ("1000", "1000"), ("01000", "01000"), ("runner:", "runner"),
                              ("run", "run"), ("runner:4002", "runner:4002"), ("1000:1000", "1000:1000")]:
        run_id = lux.submit(spec("sh", "-c", IDS, user=user))
        lux.wait_state(run_id, "succeeded")
        want = podman_ids(hosts[0], image, podman_user)
        assert ids_line(lux.logs(run_id)) == want, (user, want)
