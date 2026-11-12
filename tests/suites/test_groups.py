"""The workload runs with its user's supplementary groups, from the image's
/etc/group, as `podman run` gives them: the workload itself and an exec
into it, for the image's USER and for a user the spec names."""

from __future__ import annotations

from conftest import generic
from env import ALPINE_IMAGE

# A CI-runner-like image: its user is in two extra groups.
CONTAINERFILE = f"""FROM {ALPINE_IMAGE}
RUN addgroup -g 4001 extra1 && addgroup -g 4002 extra2 && adduser -D -u 1000 runner \\
 && addgroup runner extra1 && addgroup runner extra2 && adduser -D -u 1001 run
USER runner
"""


def spec(*cmd: str, **workload) -> dict:
    s = generic(ALPINE_IMAGE, *cmd)
    s["image"] = {"build": {"containerfile": CONTAINERFILE}}
    s["workload"].update(workload)
    return s


def test_workload_gets_the_images_groups(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(spec("sh", "-c", "echo groups=$(id -G); sleep 300"))
    lux.wait_output(run_id, "groups=")
    image = lux.get(run_id)["image"]["imageId"]
    want = hosts[0].podman("run", "--rm", image, "id", "-G").strip()
    assert want.split() == ["1000", "4001", "4002"], want
    got = next(l for l in lux.logs(run_id).splitlines() if l.startswith("groups="))
    assert got == f"groups={want}", (got, want)
    # An exec into the Run runs as the same user, with the same groups.
    out = lux.run("exec", run_id, "-T", "--", "id", "-G", input="").stdout.strip()
    assert out == want, (out, want)
    lux.run("terminate", run_id)

    # A user the spec names, by name and by uid, and one in no extra group;
    # uid:gid gets that group only, as podman gives it.
    for user, podman_user in [("runner", "runner"), ("1000", "1000"), ("run", "run"), ("runner:4002", "runner:4002")]:
        run_id = lux.submit(spec("id", "-G", user=user))
        lux.wait_state(run_id, "succeeded")
        want = hosts[0].podman("run", "--rm", "--user", podman_user, image, "id", "-G").strip()
        got = lux.logs(run_id).strip().splitlines()[-1]
        assert got == want, (user, got, want)
