"""Run size: what a Run's processes see of its size. A runner with
--cpuset pins each Run to ceil(resources.cpus) of its CPUs; containers a
nested Run starts with Podman default to the Run's memory and CPUs (a
precreate OCI hook, docs/podman.md); and real applications that size
themselves from their cgroup (ClickHouse, the JVM) size to the Run, not the
host. A runner without --cpuset behaves as before."""

from __future__ import annotations

import re

from build import CLICKHOUSE_IMAGE, DOCKER_IMAGE, JVM17_IMAGE, JVM21_IMAGE, NESTED_IMAGE
from conftest import generic
from env import ALPINE_IMAGE

GIB = 1 << 30
SIZE = {"cpus": 2, "memory": "1Gi"}
INNER = f"podman load -q -i /opt/alpine.tar >/dev/null && podman run --rm --network=none"
# One line of what the process sees: its memory.max, cpu.max, nproc and
# affinity. In a script file run inside the Run, not on the host.
SHOW = ("echo mem=$(cat /sys/fs/cgroup/memory.max) cpu=$(tr ' ' _ < /sys/fs/cgroup/cpu.max) "
        "nproc=$(nproc) aff=$(grep Cpus_allowed_list /proc/self/status | cut -f2)")
# SHOW as a file, for inner containers: written by the Run, mounted in.
SHOW_FILE = "cat > /tmp/show.sh <<'EOF'\n" + SHOW + "\nEOF\n"
SHOW_INNER = "-v /tmp/show.sh:/show.sh:ro " + ALPINE_IMAGE + " sh /show.sh"


def fields(out: str, prefix: str) -> dict[str, str]:
    """The key=value words of the output line starting with prefix."""
    line = next((l for l in out.splitlines() if l.startswith(prefix)), "")
    return dict(w.split("=", 1) for w in line.split() if "=" in w)


def nested(script: str, image: str = NESTED_IMAGE, **extra) -> dict:
    return generic(image, "sh", "-c", f"exec 2>&1; {script}", sandbox={"nestedContainers": True},
                   resources=SIZE, **extra)


def run_to_end(lux, spec: dict, timeout: float = 240) -> str:
    run_id = lux.submit(spec)
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=timeout)
    out = lux.logs(run_id)
    assert run["state"] == "succeeded", (run.get("stateReason"), out)
    return out


def host_cpus(host) -> int:
    return int(host.exec("nproc").strip())


# ---- cpuset ------------------------------------------------------------------

def test_a_run_sees_its_cpus(lux, runners, hosts):
    """With --cpuset a 2-CPU Run is pinned to 2 CPUs: nproc, its affinity,
    its cgroup's cpuset and LUX_CPUSET agree."""
    runners.start(hosts[0], "--cpuset")
    out = run_to_end(lux, generic(ALPINE_IMAGE, "sh", "-c",
                                  f"{SHOW} set=$(cat /sys/fs/cgroup/cpuset.cpus.effective) env=$LUX_CPUSET",
                                  resources=SIZE))
    f = fields(out, "mem=")
    assert f["nproc"] == "2" and f["aff"] == f["set"] == f["env"], f
    assert f["cpu"] == "200000_100000", f


def test_a_fractional_run_gets_one_cpu(lux, runners, hosts):
    runners.start(hosts[0], "--cpuset")
    out = run_to_end(lux, generic(ALPINE_IMAGE, "sh", "-c", SHOW, resources={"cpus": 0.5, "memory": "256Mi"}))
    f = fields(out, "mem=")
    assert f["nproc"] == "1" and f["cpu"] == "50000_100000", f


def test_concurrent_whole_cpu_runs_get_disjoint_sets(lux, runners, hosts):
    runners.start(hosts[0], "--cpuset")
    ids = [lux.submit(generic(ALPINE_IMAGE, "sh", "-c", f"echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
           for _ in range(2)]
    sets = []
    for run_id in ids:
        out = lux.wait_output(run_id, "set=", timeout=90)
        sets.append(set(fields(out, "set=")["set"].replace("-", ",").split(",")))
    for run_id in ids:
        lux.run("terminate", run_id)
    assert all(len(s) == 2 for s in sets) and not sets[0] & sets[1], sets


def test_a_restarted_runner_keeps_the_set(lux, runners, hosts):
    """The set is in run state: a runner restart re-adopts the Run with it,
    and the next Run is pinned beside it, not over it."""
    runners.start(hosts[0], "--cpuset")
    first = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
    before = fields(lux.wait_output(first, "set=", timeout=90), "set=")["set"]
    runners.stop(hosts[0])
    runners.start(hosts[0], "--cpuset")
    lux.wait_state(first, "running")
    second = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
    after = fields(lux.wait_output(second, "set=", timeout=90), "set=")["set"]
    lux.run("exec", first, "-T", "--", "true", input="")  # still the re-adopted container
    pinned = hosts[0].podman("inspect", f"lux-{first}", "--format", "{{.HostConfig.CpusetCpus}}").strip()
    for r in (first, second):
        lux.run("terminate", r)
    assert pinned == before, (pinned, before)
    assert before != after and not set(before.split(",")) & set(after.split(",")), (before, after)


def test_without_cpuset_nothing_is_pinned(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", f"{SHOW} env=${{LUX_CPUSET:-none}}; sleep 600",
                                resources=SIZE))
    out = lux.wait_output(run_id, "mem=", timeout=90)
    pinned = hosts[0].podman("inspect", f"lux-{run_id}", "--format", "{{.HostConfig.CpusetCpus}}").strip()
    lux.run("terminate", run_id)
    f = fields(out, "mem=")
    assert pinned == "" and f["env"] == "none", (pinned, f)
    assert int(f["nproc"]) == host_cpus(hosts[0]), f


# ---- nested containers -------------------------------------------------------

def test_inner_containers_default_to_the_runs_size(lux, runners, hosts):
    """An inner Podman container without -m or --cpus has the Run's
    memory.max and CPU quota, and the Run's CPUs; explicit limits win."""
    runners.start(hosts[0], "--nested", "--cpuset")
    limit = hosts[0].memory_limit(GIB)
    out = run_to_end(lux, nested(
        SHOW_FILE +
        "echo \"run: $(sh /tmp/show.sh)\"; "
        f"echo \"none: $({INNER} {SHOW_INNER})\"; "
        f"echo \"flags: $(podman run --rm --network=none -m 512m --cpus 1 {SHOW_INNER})\"; "
        f"echo \"info: cpus=$(podman info --format '{{{{.Host.CPUs}}}}')\""))
    run, none, flags = fields(out, "run:"), fields(out, "none:"), fields(out, "flags:")
    assert run["mem"] == str(limit) and run["nproc"] == "2", out
    assert none["mem"] == str(limit) and none["cpu"] == "200000_100000" and none["nproc"] == "2", out
    assert none["aff"] == run["aff"], out
    assert flags["mem"] == str(512 << 20) and flags["cpu"] == "100000_100000", out
    assert fields(out, "info:")["cpus"] == "2", out


DOCKERD = """
export XDG_RUNTIME_DIR=/tmp/xdg DOCKER_HOST=unix:///tmp/xdg/docker.sock; mkdir -p /tmp/xdg
dockerd-rootless >/tmp/dockerd.log 2>&1 &
for i in $(seq 90); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker load -q -i /opt/alpine.tar >/dev/null
"""


def test_inner_docker_containers_see_the_runs_size(lux, runners, hosts):
    """Rootless Docker in a Run (cgroup driver none) leaves its containers in
    the Run's cgroup: at their cgroup namespace's root they see the Run's
    memory.max and quota, and the Run's CPUs."""
    runners.start(hosts[0], "--nested", "--cpuset")
    limit = hosts[0].memory_limit(GIB)
    out = run_to_end(lux, nested(SHOW_FILE + DOCKERD + f"echo \"inner: $(docker run --rm --network=none {SHOW_INNER})\"; "
                                 "echo \"info: cpus=$(docker info --format '{{.NCPU}}')\"", image=DOCKER_IMAGE))
    inner = fields(out, "inner:")
    assert inner["mem"] == str(limit) and inner["cpu"] == "200000_100000" and inner["nproc"] == "2", out
    assert fields(out, "info:")["cpus"] == "2", out


# ---- real applications -------------------------------------------------------

CH = r"""
CH=/tmp/ch; mkdir -p $CH && cd $CH
clickhouse server --config-file=/etc/clickhouse-server/config.xml -- --path=$CH/ --tmp_path=$CH/tmp/ \
  --user_files_path=$CH/uf/ --format_schema_path=$CH/fs/ --logger.console=0 --logger.log=$CH/ch.log \
  --logger.errorlog=$CH/err.log --listen_host=127.0.0.1 >$CH/out.log 2>&1 &
for i in $(seq 600); do clickhouse client -q 'SELECT 1' >/dev/null 2>&1 && break; kill -0 $! || break; sleep 0.5; done
clickhouse client -q "SELECT 'ch: mem=' || value || ' threads=' || toString(getSetting('max_threads')) FROM system.server_settings WHERE name='max_server_memory_usage'" \
  || { grep -m3 Exception $CH/err.log; exit 1; }
kill $!
"""

JVM = ("java -XX:+PrintFlagsFinal -XshowSettings:system -version 2>&1 | "
       "awk '/ MaxHeapSize /{h=$4} /Effective CPU Count/{c=$4} END{print \"jvm: heap=\" h \" cpus=\" c}'")


def assert_sized(out: str, prefix: str, limit: int, cpus: int) -> None:
    f = fields(out, prefix)
    if prefix == "ch:":
        # ClickHouse keeps max_server_memory_usage at 0.9 of what it sees.
        assert abs(int(f["mem"]) - 0.9 * limit) < 0.01 * limit and f["threads"] == str(cpus), (f, limit, out)
    else:
        # The JVM's default heap is a quarter of what it sees.
        assert abs(int(f["heap"]) - limit / 4) < 0.02 * limit and f["cpus"] == str(cpus), (f, limit, out)


def test_clickhouse_and_the_jvm_size_to_a_plain_run(lux, runners, hosts):
    runners.start(hosts[0], "--cpuset")
    limit = hosts[0].memory_limit(GIB)
    ch = run_to_end(lux, generic(CLICKHOUSE_IMAGE, "sh", "-c", CH, resources=SIZE), timeout=420)
    assert_sized(ch, "ch:", limit, 2)
    for image in (JVM21_IMAGE, JVM17_IMAGE):
        assert_sized(run_to_end(lux, generic(image, "sh", "-c", JVM, resources=SIZE)), "jvm:", limit, 2)
