"""Spike only (.spike/lxcfs.md): LXCFS so Runs, and the containers nested
Runs start, see the Run's CPU and memory. Each step is one test; what it
saw is written to .spike/out/<test>.log for the report, and the assertions
are loose (a spike records, it does not gate)."""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from conftest import generic
from env import ALPINE_IMAGE, Host

pytestmark = pytest.mark.timeout(1200)

OUT = Path(__file__).resolve().parents[2] / ".spike" / "out"
LXCFS_DIR = "/var/lib/lxcfs"
SIZE = {"cpus": 2, "memory": "1Gi"}

LXCFS_ENV = {"LUX_SPIKE_LXCFS": LXCFS_DIR}
HIDDEN_ENV = {**LXCFS_ENV, "LUX_SPIKE_HIDDEN_PROC": "1", "LUX_SPIKE_HIDDEN_SYS": "1"}


def image(env) -> str:
    return env.extra["images"]["lxcfs"]


def save(name: str, text: str) -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / f"{name}.log").write_text(text)
    print(f"===== {name}\n{text}")


def start_lxcfs(host: Host, *flags: str) -> str:
    # LXCFS takes its cgroup2 hierarchy from pid 1's cgroup and the
    # controllers listed in its cgroup.controllers. On a systemd host that is
    # / (pid 1 in /init.scope, pruned). Here pid 1 is `sleep` in the
    # container's root, whose children get controllers only once Podman has
    # enabled them: do what Podman would, before LXCFS starts.
    prep = host.exec("sh", "-c", """
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo $p > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null; done
for c in $(cat /sys/fs/cgroup/cgroup.controllers); do echo +$c > /sys/fs/cgroup/cgroup.subtree_control; done
echo "pid1 cgroup=$(cut -d: -f3 /proc/1/cgroup) controllers there=$(cat /sys/fs/cgroup$(cut -d: -f3 /proc/1/cgroup)/cgroup.controllers)"
""", check=False)
    # Unconditional: on a dead daemon `mountpoint` fails with ENOTCONN.
    host.exec("sh", "-c", f"mkdir -p {LXCFS_DIR}; fusermount3 -u {LXCFS_DIR} 2>/dev/null || umount -l {LXCFS_DIR} 2>/dev/null; true", check=False)
    host.exec("sh", "-c", f"lxcfs {' '.join(flags)} {LXCFS_DIR} >>/var/log/lxcfs.log 2>&1 &")
    for _ in range(50):
        if host.exec("sh", "-c", f"test -e {LXCFS_DIR}/proc/meminfo && echo up", check=False).strip() == "up":
            break
        time.sleep(0.2)
    else:
        raise AssertionError("lxcfs did not come up: " + host.exec("sh", "-c", "tail -5 /var/log/lxcfs.log", check=False))
    return prep + host.exec("sh", "-c", "lxcfs --version; pgrep -a lxcfs; grep -A1 hierarchies: /var/log/lxcfs.log | tail -1", check=False)


# poststart: the container's mount namespace has pivoted, so setns into it
# resolves /dev/.lux in the container's root. The shim waits for the
# runner's handshake before it starts the workload, so this lands first.
def set_hook(host: Host, stage: str = "poststart") -> None:
    host.exec("sh", "-c", "mkdir -p /usr/share/containers/oci/hooks.d && "
              f"sed s/STAGE/{stage}/g /usr/local/share/lux-spike-hook.json.in > /usr/share/containers/oci/hooks.d/lux-spike.json")


def run(lux, env, script: str, nested: bool, timeout: float = 240, **extra) -> tuple[dict, str]:
    sandbox = {"sandbox": {"nestedContainers": True}} if nested else {}
    spec = generic(image(env), "sh", "-c", f"exec 2>&1; {script}", resources=SIZE, **sandbox, **extra)
    run_id = lux.submit(spec)
    r = lux.wait_state(run_id, "succeeded", "failed", timeout=timeout)
    return r, lux.logs(run_id)


def host_probe(host: Host) -> str:
    probe = (Path(__file__).resolve().parents[1] / "images" / "lxcfs" / "probe.sh").read_bytes()
    return host.exec("sh", "-s", "simhost", input=probe, check=False)


@pytest.fixture
def lxcfs_host(hosts):
    h = hosts[0]
    yield h
    h.exec("sh", "-c", f"pkill -x lxcfs; sleep 1; fusermount3 -u {LXCFS_DIR}; rm -f /run/lxcfs.pid /usr/share/containers/oci/hooks.d/lux-spike.json; true",
           check=False)


# Inner Podman: loads alpine, then runs the probe in a container with no
# limits and one with -m 256m --cpus 1. Extra podman args via $IA.
INNER_PODMAN = f"""
podman load -q -i /opt/alpine.tar >/dev/null || echo INNER-LOAD-FAILED
echo "P:podman-info:cpus_mem=$(podman info --format '{{{{.Host.CPUs}}}}_{{{{.Host.MemTotal}}}}' 2>&1)"
podman run --rm --network=none $IA -v /opt/probe.sh:/p.sh:ro {ALPINE_IMAGE} sh /p.sh podman-inner || echo INNER-PODMAN-FAILED
podman run --rm --network=none $IA -m 256m --cpus 1 -v /opt/probe.sh:/p.sh:ro {ALPINE_IMAGE} sh /p.sh podman-inner-limited || echo INNER-PODMAN-LIMITED-FAILED
"""

DOCKERD = """
export XDG_RUNTIME_DIR=/tmp/xdg DOCKER_HOST=unix:///tmp/xdg/docker.sock; mkdir -p /tmp/xdg
dockerd-rootless >/tmp/dockerd.log 2>&1 &
for i in $(seq 90); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || { echo DOCKERD-FAILED; tail -30 /tmp/dockerd.log; }
"""

INNER_DOCKER = DOCKERD + f"""
echo "P:docker-info:NCPU_MemTotal=$(docker info --format '{{{{.NCPU}}}}_{{{{.MemTotal}}}}' 2>&1)"
docker load -q -i /opt/alpine.tar >/dev/null || echo DOCKER-LOAD-FAILED
docker run --rm --network=none $DA -v /opt/probe.sh:/p.sh:ro {ALPINE_IMAGE} sh /p.sh docker-inner || echo INNER-DOCKER-FAILED
docker run --rm --network=none $DA -m 256m --cpus 1 -v /opt/probe.sh:/p.sh:ro {ALPINE_IMAGE} sh /p.sh docker-inner-limited || echo INNER-DOCKER-LIMITED-FAILED
grep -iE 'error|fail' /tmp/dockerd.log | tail -5 | sed 's/^/DOCKERD-LOG: /'
"""

# Files LXCFS virtualises, bound into inner containers from the Run's
# /proc (which are themselves the host's LXCFS files).
LXCFS_FILES = ["meminfo", "cpuinfo", "stat", "uptime", "loadavg", "diskstats", "swaps"]
INNER_BINDS = " ".join(f"-v /proc/{f}:/proc/{f}:ro" for f in LXCFS_FILES) + \
    " -v /sys/devices/system/cpu/online:/sys/devices/system/cpu/online:ro"

HIDDEN_VIEW = """
echo "P:hidden:mount=$(grep -E ' /dev/\\.lux/' /proc/self/mountinfo | tr '\\n' '|')"
echo "P:hidden:proc.meminfo=$(awk '/^MemTotal:/{print $2$3}' /dev/.lux/proc/meminfo 2>&1)"
echo "P:hidden:proc.cpuinfo=$(grep -c ^processor /dev/.lux/proc/cpuinfo 2>&1)"
echo "P:hidden:proc.pid1=$(cat /dev/.lux/proc/1/comm 2>&1) entries=$(ls /dev/.lux/proc | grep -c '^[0-9]')"
for f in kcore keys timer_list sched_debug sysrq-trigger acpi scsi latency_stats; do
  echo "P:hidden:$f proc=$(head -c1 /proc/$f >/dev/null 2>&1 && echo readable || echo no) hidden=$(head -c1 /dev/.lux/proc/$f >/dev/null 2>&1 && echo readable || echo no)"
done
echo "P:hidden:proc.sys.rw proc=$(grep -E ' /proc/sys ' /proc/self/mountinfo | awk '{print $6}') hidden-write-hostname=$(echo x > /dev/.lux/proc/sys/kernel/hostname 2>&1 && echo WROTE || echo refused)"
echo "P:hidden:sys.firmware proc=$(ls /sys/firmware 2>&1 | head -3 | tr '\\n' ' ') hidden=$(ls /dev/.lux/sys/firmware 2>&1 | head -3 | tr '\\n' ' ')"
echo "P:hidden:sys.cpu.online=$(cat /dev/.lux/sys/devices/system/cpu/online 2>&1)"
echo "P:hidden:sys.fs.cgroup=$(ls /dev/.lux/sys/fs/cgroup 2>&1 | head -3 | tr '\\n' ' ')"
"""


def test_step0_host(lxcfs_host):
    """What the simulated host itself shows, and the lxcfs build."""
    h = lxcfs_host
    out = host_probe(h)
    out += h.exec("sh", "-c", "rpm -q lxcfs fuse3-libs; lxcfs --help 2>&1; uname -r", check=False)
    save("step0_host", out)


def test_step1_plain(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = "## baseline (no LXCFS)\n"
    runners.start(h)
    r, logs = run(lux, env, "/opt/probe.sh run", nested=False)
    out += logs
    runners.stop(h)
    out += "\n## lxcfs\n" + start_lxcfs(h, "--enable-cfs", "-l")
    out += host_probe(h).replace("P:simhost", "P:simhost-after-lxcfs")
    out += h.exec("sh", "-c", f"grep -c ^processor {LXCFS_DIR}/proc/cpuinfo; head -2 {LXCFS_DIR}/proc/meminfo", check=False)
    for sysmode in ("online", "cpu"):
        runners.start(h, environ={**LXCFS_ENV, "LUX_SPIKE_LXCFS_SYS": sysmode})
        r, logs = run(lux, env, "/opt/probe.sh run", nested=False)
        out += f"\n## overmounts, sys={sysmode}: state={r['state']} {r.get('stateReason', '')}\n" + logs
        runners.stop(h)
    save("step1_plain", out)


GLIBC = f"""
exec 2>&1
# No internal processes: everything in the root goes to /init first.
mkdir -p /sys/fs/cgroup/init; for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo $p > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null; done
echo +cpu > /sys/fs/cgroup/cgroup.subtree_control
mkdir -p /sys/fs/cgroup/spike && echo '200000 100000' > /sys/fs/cgroup/spike/cpu.max && echo $$ > /sys/fs/cgroup/spike/cgroup.procs
echo "G:cgroup=$(cat /proc/self/cgroup) cpu.max=$(cat /sys/fs/cgroup/spike/cpu.max)"
unshare -m -p -f sh -c '
mount --bind {LXCFS_DIR}/sys/devices/system/cpu/online /sys/devices/system/cpu/online
mount --bind {LXCFS_DIR}/proc/cpuinfo /proc/cpuinfo
echo "G:ldd=$(ldd --version | head -1)"
echo "G:direct online=$(cat {LXCFS_DIR}/sys/devices/system/cpu/online) cpuinfo=$(grep -c ^processor {LXCFS_DIR}/proc/cpuinfo) pidns=$(readlink /proc/self/ns/pid) mypid=$$ cg=$(cat /proc/self/cgroup)"
echo "G:online=$(cat /sys/devices/system/cpu/online) cpuinfo=$(grep -c ^processor /proc/cpuinfo)"
echo "G:nproc=$(nproc) getconf=$(getconf _NPROCESSORS_ONLN) getconf_conf=$(getconf _NPROCESSORS_CONF)"
echo "G:python.cpu_count=$(python3 -c "import os;print(os.cpu_count())") affinity=$(python3 -c "import os;print(len(os.sched_getaffinity(0)))")"
echo 0-1 > /run/fake-online
mount --bind /run/fake-online /sys/devices/system/cpu/online
echo "G:with a plain file 0-1 over online: getconf=$(getconf _NPROCESSORS_ONLN) getconf_conf=$(getconf _NPROCESSORS_CONF) python.cpu_count=$(python3 -c "import os;print(os.cpu_count())") nproc=$(nproc) nproc_all=$(nproc --all)"
'
echo $$ > /sys/fs/cgroup/cgroup.procs 2>/dev/null || echo $$ > /sys/fs/cgroup/init/cgroup.procs; rmdir /sys/fs/cgroup/spike
"""


def test_step1b_glibc(lxcfs_host):
    h = lxcfs_host
    # Restart LXCFS after the cgroup layout is made (it is in /init then).
    out = h.exec("sh", "-c", GLIBC.split("unshare")[0] + "echo $$ > /sys/fs/cgroup/init/cgroup.procs", check=False)
    out += start_lxcfs(h, "--enable-cfs", "-l")
    out += h.exec("sh", "-c", GLIBC, check=False)
    save("step1b_glibc", out)


DIAG = """
c=$(podman ps -q --filter label=lux.managed=true | head -1)
pid=$(podman inspect -f '{{.State.Pid}}' $c)
echo "D:shim pid=$pid cgroup=$(cat /proc/$pid/cgroup)"
cg=$(cut -d: -f3 /proc/$pid/cgroup)
d=$cg; while :; do
  echo "D:level $d memory.max=$(cat /sys/fs/cgroup$d/memory.max 2>&1) cpu.max=$(cat /sys/fs/cgroup$d/cpu.max 2>&1) controllers=$(cat /sys/fs/cgroup$d/cgroup.controllers 2>&1) procs=$(wc -l < /sys/fs/cgroup$d/cgroup.procs 2>&1)"
  [ "$d" = / ] || [ -z "$d" ] && break; d=$(dirname $d); done
for p in $(cat /sys/fs/cgroup$(dirname $cg)/workload/main/cgroup.procs 2>/dev/null | head -2); do echo "D:workload pid $p cgroup=$(cat /proc/$p/cgroup)"; done
echo "D:lxcfs cgroup=$(cat /proc/$(pgrep -x lxcfs)/cgroup)"
for p in $pid $(cat /sys/fs/cgroup$(dirname $cg)/workload/main/cgroup.procs 2>/dev/null | head -1); do
  echo "D:pidns of $p=$(readlink /proc/$p/ns/pid) status.NSpid=$(grep NSpid /proc/$p/status)"
  echo "D:via nsenter -p into $p: $(nsenter -t $p -p -- awk '/^MemTotal/{print $2}' /var/lib/lxcfs/proc/meminfo) cpus=$(nsenter -t $p -p -- grep -c ^processor /var/lib/lxcfs/proc/cpuinfo)"
done
echo "D:lux.shim memory.current=$(cat /sys/fs/cgroup$cg/memory.current 2>&1) stat=$(head -1 /sys/fs/cgroup$cg/memory.stat 2>&1)"
# A reader of the LXCFS files placed in a given cgroup, as pid 1 of a new
# pid namespace (as a container's init is) and not.
run=$(dirname $cg)
mkdir -p /sys/fs/cgroup$run/workload/spike-a /sys/fs/cgroup/spike-top
echo "D:root subtree_control=$(cat /sys/fs/cgroup/cgroup.subtree_control)"
echo '300000 100000' > /sys/fs/cgroup/spike-top/cpu.max; echo 536870912 > /sys/fs/cgroup/spike-top/memory.max
sh /usr/local/bin/lxcfs-reader.sh /var/lib/lxcfs $run/lux.shim $run/workload/main $run/workload/spike-a /spike-top /init
rmdir /sys/fs/cgroup$run/workload/spike-a /sys/fs/cgroup/spike-top 2>&1 | sed 's/^/D:rmdir /'
grep -vE '^ *(unique|dev unique|getattr|open|read|flush|release|   )|^(open|read|flush|release|getattr|lookup|opendir|readdir|releasedir|LOOKUP|access)' /var/log/lxcfs.log | sort | uniq -c | sort -rn | head -40 | sed 's/^/D:lxcfs.other /'
tail -20 /var/log/lxcfs.log | sed 's/^/D:lxcfs.log /'
grep -iE 'fail|error|denied|No such' /var/log/lxcfs.log | sort | uniq -c | sort -rn | head -20 | sed 's/^/D:lxcfs.err /'
"""


def test_step2b_diag(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l", "-d")
    runners.start(h, "--nested", environ=LXCFS_ENV)
    spec = generic(image(env), "sh", "-c", "exec 2>&1; /opt/probe.sh run; echo READY; sleep 600", resources=SIZE,
                   sandbox={"nestedContainers": True})
    rid = lux.submit(spec)
    out += lux.wait_output(rid, "READY", timeout=120)
    out += h.exec("sh", "-c", DIAG, check=False)
    lux.run("terminate", rid, "--wait")
    save("step2b_diag", out)


def test_step2_nested_overmounts(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l")
    runners.start(h, "--nested")
    r, logs = run(lux, env, "IA=; " + INNER_PODMAN, nested=True)
    out += f"\n## baseline nested, no LXCFS: podman: {r['state']}\n" + logs
    runners.stop(h)
    runners.start(h, "--nested", environ=LXCFS_ENV)
    r, logs = run(lux, env, "/opt/probe.sh run; IA=; " + INNER_PODMAN, nested=True)
    out += f"\n## nested + overmounts: podman: {r['state']}\n" + logs
    r, logs = run(lux, env, "DA=; " + INNER_DOCKER, nested=True)
    out += f"\n## nested + overmounts: docker: {r['state']}\n" + logs
    save("step2_nested_overmounts", out)


def test_step3_hidden_procfs(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l")
    set_hook(h)
    # Hidden procfs only, then procfs + sysfs.
    for name, environ in (("proc", {**LXCFS_ENV, "LUX_SPIKE_HIDDEN_PROC": "1"}), ("proc+sys", HIDDEN_ENV)):
        runners.start(h, "--nested", environ=environ)
        r, logs = run(lux, env, "/opt/probe.sh run; " + HIDDEN_VIEW + "IA=; " + INNER_PODMAN, nested=True)
        out += f"\n## nested + overmounts + hidden {name}: podman: {r['state']}\n" + logs
        r, logs = run(lux, env, "DA=; " + INNER_DOCKER, nested=True)
        out += f"\n## nested + overmounts + hidden {name}: docker: {r['state']}\n" + logs
        runners.stop(h)
    # What a hidden procfs would expose in a Run that is not nested (masked paths).
    runners.start(h, environ=HIDDEN_ENV)
    r, logs = run(lux, env, HIDDEN_VIEW, nested=False)
    out += f"\n## NOT nested + hidden proc+sys (exposure check): {r['state']}\n" + logs
    out += "\n## hook log\n" + h.exec("sh", "-c", "cat /var/log/lux-spike-hook.log", check=False)
    save("step3_hidden_procfs", out)


def test_step4_inner_limits(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l")
    set_hook(h)
    runners.start(h, "--nested", environ=HIDDEN_ENV)
    # Podman: per-container -v.
    r, logs = run(lux, env, f"IA='{INNER_BINDS}'; " + INNER_PODMAN, nested=True)
    out += f"\n## podman inner, -v binds: {r['state']}\n" + logs
    # Podman: containers.conf default volumes, no per-container flag.
    conf = "[containers]\\nvolumes = [" + ", ".join(
        f'\\"/proc/{f}:/proc/{f}:ro\\"' for f in LXCFS_FILES) + ', \\"/sys/devices/system/cpu/online:/sys/devices/system/cpu/online:ro\\"]\\n'
    r, logs = run(lux, env, f"mkdir -p ~/.config/containers; printf \"{conf}\" > ~/.config/containers/containers.conf; "
                  "cat ~/.config/containers/containers.conf; IA=; " + INNER_PODMAN, nested=True)
    out += f"\n## podman inner, containers.conf volumes: {r['state']}\n" + logs
    # Docker: per-container -v.
    r, logs = run(lux, env, f"DA='{INNER_BINDS}'; " + INNER_DOCKER, nested=True)
    out += f"\n## docker inner, -v binds: {r['state']}\n" + logs
    save("step4_inner_limits", out)


READ_LOOP = """
i=0; while :; do i=$((i+1)); s=$(date +%s.%N)
  v=$(timeout 5 awk '/^MemTotal:/{print $2}' /proc/meminfo 2>&1); rc=$?
  echo "R:$i rc=$rc t=$(date +%T) v=$v" | tr '\\n' ' '; echo
  sleep 1; done
"""


def test_step5_failure(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l")
    try:
        out = _step5(lux, env, runners, h, out)
    finally:
        save("step5_failure", out)


def _step5(lux, env, runners, h, out):
    set_hook(h)
    runners.start(h, "--nested", environ=HIDDEN_ENV)
    spec = generic(image(env), "sh", "-c", "exec 2>&1; " + READ_LOOP, resources=SIZE)
    rid = lux.submit(spec)
    lux.wait_output(rid, "R:3 ", timeout=120)
    host = lambda c: h.exec("sh", "-c", c, check=False)  # noqa: E731
    out += "\n## SIGUSR1 (reload liblxcfs)\n" + host("pkill -USR1 -x lxcfs; sleep 3; tail -3 /var/log/lxcfs.log")
    out += f"\n## SIGSTOP (hung daemon) at {host('date +%T')}" + host(
        f"pkill -STOP -x lxcfs; s=$(date +%s.%N); timeout 5 cat {LXCFS_DIR}/proc/meminfo >/dev/null; echo \"host read rc=$? after $(awk -v s=$s -v e=$(date +%s.%N) 'BEGIN{{print e-s}}')s\"; "
        "sleep 8; pkill -CONT -x lxcfs; sleep 3")
    out += f"\n## SIGKILL at {host('date +%T')}" + host(
        f"pkill -KILL -x lxcfs; sleep 4; cat {LXCFS_DIR}/proc/meminfo 2>&1 | head -1; grep lxcfs /proc/mounts")
    # A new Run while the FUSE mount is dead.
    r, logs = run(lux, env, "/opt/probe.sh dead", nested=False, timeout=120)
    out += f"\n## new Run while lxcfs is dead: {r['state']} {r.get('stateReason', '')}\n" + logs[-1500:]
    out += "\n## runner.log placement failures\n" + "".join(
        l for l in (Path(h.log_dir) / "runner.log").read_text().splitlines(True) if "placement failed" in l)[-1500:]
    out += f"\n## restart lxcfs at {host('date +%T')} (unmount the dead mount first)\n" + start_lxcfs(h, "--enable-cfs", "-l")
    time.sleep(4)
    r, logs = run(lux, env, "/opt/probe.sh restarted", nested=False, timeout=120)
    out += f"\n## new Run after restart: {r['state']} {r.get('stateReason', '')}\n" + logs[-1500:]
    time.sleep(3)
    out += "\n## the long-running Run's reads, whole\n" + lux.logs(rid)
    lux.run("terminate", rid, "--wait")
    return out


BENCH = """
python3 - <<'EOF'
import time
for path in ("/proc/meminfo", "/dev/.lux/proc/meminfo", "/proc/cpuinfo", "/dev/.lux/proc/cpuinfo", "/proc/stat", "/dev/.lux/proc/stat"):
    try:
        t = time.perf_counter()
        for _ in range(1000):
            with open(path, "rb") as f:
                f.read()
        print(f"B:{path} {(time.perf_counter()-t)*1000:.0f}ms/1000 reads")
    except OSError as e:
        print(f"B:{path} {e}")
EOF
"""


def test_step6_cost(lux, env, runners, lxcfs_host):
    h = lxcfs_host
    out = start_lxcfs(h, "--enable-cfs", "-l")
    set_hook(h)
    out += "\n## rss before\n" + h.exec("sh", "-c", "ps -o pid,rss,vsz,nlwp,cmd -C lxcfs", check=False)
    runners.start(h, "--nested", environ=HIDDEN_ENV)
    r, logs = run(lux, env, BENCH, nested=True)
    out += f"\n## bench: {r['state']}\n" + logs
    out += "\n## rss after\n" + h.exec("sh", "-c", "ps -o pid,rss,vsz,nlwp,cmd -C lxcfs", check=False)
    save("step6_cost", out)
