"""Spike: per-container limits for Docker-CLI workloads in a nested Run.

Each test starts one engine variant per Run, inside a real nested Run with
the shim's cgroup delegation, and runs tests/images/spike/matrix.sh against
it with the docker CLI. Rows are reported, not asserted: the raw output of
each Run is written to .spike/raw/<variant>.log at the repository root."""

from __future__ import annotations

from pathlib import Path

from build import SPIKE_IMAGE
from conftest import generic

ROOT = Path(__file__).resolve().parents[2]
MATRIX = (ROOT / "tests" / "images" / "spike" / "matrix.sh").read_text()

COMMON = """
exec 2>&1
export XDG_RUNTIME_DIR=/tmp/xdg; mkdir -p /tmp/xdg
wait_engine() { for i in $(seq 90); do docker info >/dev/null 2>&1 && return 0; sleep 1; done
  echo ENGINE-FAILED; tail -40 "$ENGINE_LOG"; exit 1; }
"""

# Stock rootless dockerd, as tests/images/docker runs it: the baseline.
DOCKERD_STOCK = """
export DOCKER_HOST=unix:///tmp/xdg/docker.sock ENGINE_LOG=/tmp/dockerd.log
export DOCKERD_ROOTLESS_ROOTLESSKIT_NET=pasta DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=implicit
dockerd-rootless >$ENGINE_LOG 2>&1 &
wait_engine
"""

# Rootless dockerd that does not call itself rootless: RootlessKit still
# makes the user, mount and network namespaces, but dockerd runs without
# RootlessKit's environment (tests/images/spike/dockerd-userns), the only
# thing its rootless mode keys on, so cgroupDriver() is cgroupfs, not none,
# and containers' cgroups are made under the delegated /workload. Not in
# rootless mode it cannot use a detached netns or RootlessKit's port API, nor
# the rootless default paths: all given explicitly.
DOCKERD_CGROUPFS = """
export DOCKER_HOST=unix:///tmp/xdg/docker.sock ENGINE_LOG=/tmp/dockerd.log
export DOCKERD=dockerd-userns DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false BX_CGROUP_PARENT=/workload/buildx
export DOCKERD_ROOTLESS_ROOTLESSKIT_NET=pasta DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=implicit
dockerd-rootless --exec-opt native.cgroupdriver=cgroupfs --cgroup-parent=/workload \\
  --data-root $HOME/.local/share/docker --exec-root /tmp/xdg/docker -H $DOCKER_HOST \\
  --pidfile /tmp/xdg/docker.pid >$ENGINE_LOG 2>&1 &
wait_engine
"""

# DOCKERD_CGROUPFS with tests/images/spike/runc-ro-sysfs as the default
# runtime: every sysfs mount read-only, as the Run's own /sys is.
DOCKERD_CGROUPFS_RO_SYSFS = DOCKERD_CGROUPFS.replace(
    "--cgroup-parent=/workload", "--cgroup-parent=/workload --add-runtime ro-sysfs=/usr/local/bin/runc-ro-sysfs "
    "--default-runtime ro-sysfs")

# And re-rooting any cgroup path outside /workload (buildx's /docker/buildx)
# below it. The runtime is named by a path: containerd's shim does not pass
# dockerd's environment to it.
DOCKERD_CGROUPFS_REROOT = (
    "printf '#!/bin/sh\\nRO_SYSFS_REROOT=/workload exec /usr/local/bin/runc-ro-sysfs \"$@\"\\n' > /tmp/xdg/runc-reroot\n"
    "chmod 755 /tmp/xdg/runc-reroot\n"
    + DOCKERD_CGROUPFS.replace(
        "--cgroup-parent=/workload", "--cgroup-parent=/workload --add-runtime reroot=/tmp/xdg/runc-reroot "
        "--default-runtime reroot").replace(" BX_CGROUP_PARENT=/workload/buildx", ""))

# Podman's Docker-compatible API, the docker CLI pointed at it.
PODMAN_API = """
export DOCKER_HOST=unix:///tmp/xdg/podman.sock ENGINE_LOG=/tmp/podman.log
podman system service --time=0 unix:///tmp/xdg/podman.sock >$ENGINE_LOG 2>&1 &
export ENGINE_PID=$!
wait_engine
podman load -q -i /opt/alpine.tar >/dev/null; podman load -q -i /opt/buildkit.tar >/dev/null
"""

# Rootless containerd (nerdctl's containerd-rootless.sh: RootlessKit with
# slirp4netns and the builtin port driver), BuildKit's containerd worker
# entered into its namespaces, and the docker CLI's name for nerdctl. nerdctl
# in rootless mode without systemd defaults to cgroup manager "none";
# nerdctl.toml sets cgroupfs. containerd's default cgroup path is
# /<namespace>/<id>, outside /workload: containerd finds tests/images/spike/
# runc-ro-sysfs as `runc` first in its PATH, re-rooting it. Its store is on
# the runner's engine volume (~/.local/share/docker): elsewhere in the
# container's overlay root there is no kernel overlay.
NERDCTL = """
export ENGINE_LOG=/tmp/containerd.log
mkdir -p /tmp/xdg/rbin /tmp/xdg/cbin ~/.config/nerdctl ~/.config/containerd
printf '#!/bin/sh\\nRO_SYSFS_REROOT=/workload exec /usr/local/bin/runc-ro-sysfs "$@"\\n' > /tmp/xdg/rbin/runc
chmod 755 /tmp/xdg/rbin/runc
ln -sf /usr/bin/nerdctl /tmp/xdg/cbin/docker
printf 'cgroup_manager = "cgroupfs"\\n' > ~/.config/nerdctl/nerdctl.toml
store=$HOME/.local/share/docker/c
mkdir -p $store
PATH=/tmp/xdg/rbin:$PATH XDG_DATA_HOME=$store containerd-rootless.sh >$ENGINE_LOG 2>&1 &
export PATH=/tmp/xdg/cbin:$PATH
for i in $(seq 60); do [ -s /tmp/xdg/containerd-rootless/child_pid ] && break; sleep 1; done
wait_engine
nsenter -U --preserve-credentials -m -n -t $(cat /tmp/xdg/containerd-rootless/child_pid) \\
  buildkitd --oci-worker=false --containerd-worker=true --containerd-worker-rootless=true \\
  --containerd-worker-addr=/run/containerd/containerd.sock --root $store/buildkit \\
  --addr unix:///tmp/xdg/buildkit-default/buildkitd.sock >/tmp/buildkitd.log 2>&1 &
for i in $(seq 30); do [ -S /tmp/xdg/buildkit-default/buildkitd.sock ] && break; sleep 1; done
echo "buildkitd: $(ls /tmp/xdg/buildkit-default 2>&1)"; tail -3 /tmp/buildkitd.log
"""

# nerdctl build with the base from an OCI layout (no registry), and the
# BuildKit step's cgroup.
NERDCTL_POST = """
dg=$(grep -o '"digest":"sha256:[0-9a-f]*"' /tmp/oci/index.json 2>/dev/null | head -1 | cut -d'"' -f4)
[ -n "$dg" ] || { mkdir -p /tmp/oci && tar -xf /opt/alpine.tar -C /tmp/oci; dg=$(grep -o '"digest":"sha256:[0-9a-f]*"' /tmp/oci/index.json | head -1 | cut -d'"' -f4); }
printf 'FROM base\\nRUN echo one > /one\\nRUN echo "step-cgroup=$(cat /proc/self/cgroup)" > /two\\nCMD cat /one /two\\n' > /tmp/b4/Dockerfile.oci
nerdctl build --progress plain -t spike-oci -f /tmp/b4/Dockerfile.oci --build-context "base=oci-layout:///tmp/oci@$dg" /tmp/b4 >/tmp/row.out 2>&1
echo "ROW4-offline $(nerdctl run --rm spike-oci 2>&1 | tr '\\n' +)"; tail -15 /tmp/row.out | sed 's/^/    | /'
echo "buildkitd log:"; tail -10 /tmp/buildkitd.log | sed 's/^/    > /'
echo "nerdctl inspect m1-like:"; nerdctl run --name m9 --network none -m 64m docker.io/library/alpine:3.24.2 sh -c 'x=a; while :; do x=$x$x; done' >/dev/null 2>&1; echo "m9 rc=$?"
nerdctl inspect m9 --format '{{json .State}}' 2>&1 | cut -c1-400; nerdctl rm -f m9 >/dev/null 2>&1
"""

# The docker CLI's builder against an engine without Moby's BuildKit API is
# a docker-container builder of buildx's default image: give it that tag.
PODMAN_API_DEFAULT_BUILDKIT = PODMAN_API + """
podman tag docker.io/moby/buildkit:v0.33.1 docker.io/moby/buildkit:buildx-stable-1
export BX_CGROUP_PARENT=/workload/buildx
"""


def spike(lux, runners, hosts, name: str, engine: str, extra: str = "", post: str = "", timeout: int = 900) -> str:
    runners.start(hosts[0], "--nested")
    run_id = lux.submit(generic(SPIKE_IMAGE, "sh", "-c", COMMON + engine + extra + MATRIX + post,
                                sandbox={"nestedContainers": True},
                                resources={"cpus": 2, "memory": "1Gi"}))
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=timeout)
    out = lux.logs(run_id)
    raw = ROOT / ".spike" / "raw"
    raw.mkdir(parents=True, exist_ok=True)
    (raw / f"{name}.log").write_text(f"state={run['state']} reason={run.get('stateReason')}\n{out}")
    print(f"== {name}\n{out}")
    return out


def test_a0_dockerd_stock(lux, runners, hosts):
    spike(lux, runners, hosts, "a0-dockerd-stock", DOCKERD_STOCK)


def test_a1_dockerd_cgroupfs(lux, runners, hosts):
    spike(lux, runners, hosts, "a1-dockerd-cgroupfs", DOCKERD_CGROUPFS, "export PROBE_PRIV=1\n")


def test_a2_dockerd_cgroupfs_ro_sysfs(lux, runners, hosts):
    spike(lux, runners, hosts, "a2-dockerd-cgroupfs-ro-sysfs", DOCKERD_CGROUPFS_RO_SYSFS, "export PROBE_PRIV=1\n")


def test_a3_dockerd_cgroupfs_reroot(lux, runners, hosts):
    """Also shows where BuildKit's container and its RUN steps' cgroups land."""
    spike(lux, runners, hosts, "a3-dockerd-cgroupfs-reroot", DOCKERD_CGROUPFS_REROOT, "export BX_SHOW_CGROUP=1\n")


def test_b1_podman_api(lux, runners, hosts):
    spike(lux, runners, hosts, "b1-podman-api", PODMAN_API, "export PROBE_PRIV=1 BX_SHOW_CGROUP=1\n"
          "echo podman-direct-port:; podman run -d --name pd -p 8081:80 docker.io/library/alpine:3.24.2 "
          "sh -c 'while :; do echo pd-ok | nc -l -p 80; done' >/dev/null; sleep 2; nc -w 3 127.0.0.1 8081 </dev/null; "
          "podman rm -f pd >/dev/null\n")


def test_b2_podman_api_buildkit(lux, runners, hosts):
    spike(lux, runners, hosts, "b2-podman-api-buildkit", PODMAN_API_DEFAULT_BUILDKIT,
          "export PROBE_PRIV=1 BX_SHOW_CGROUP=1 SKIP_ROWS='1 2 3 6'\n")


def test_c1_nerdctl(lux, runners, hosts):
    """nerdctl as `docker`: compose is `nerdctl compose`, buildx does not
    exist (row 5 is expected to fail on the CLI)."""
    spike(lux, runners, hosts, "c1-nerdctl", NERDCTL, "", NERDCTL_POST)


# The build rows again, the nerdctl way: no -q, the build's progress shown,
# and an OCI-layout context without a digest.
NERDCTL_BUILD = """
mkdir -p /tmp/b4 /tmp/oci && tar -xf /opt/alpine.tar -C /tmp/oci
printf 'FROM docker.io/library/alpine:3.24.2\\nRUN echo one > /one\\nRUN echo two > /two; cat /proc/self/cgroup > /cg\\nCMD cat /one /two /cg\\n' > /tmp/b4/Dockerfile
nerdctl build --progress plain -t spike-b4 /tmp/b4 >/tmp/row.out 2>&1; echo "build rc=$?"; tail -25 /tmp/row.out | sed 's/^/    | /'
echo "ROW4-nerdctl $(nerdctl run --rm spike-b4 2>&1 | tr '\\n' +)"
nerdctl build -q -t spike-b4q /tmp/b4 >/tmp/row.out 2>&1; echo "build -q rc=$? $(tail -2 /tmp/row.out | tr '\\n' ' ')"
printf 'FROM base\\nRUN echo one > /one\\nCMD cat /one\\n' > /tmp/b4/Dockerfile.oci
nerdctl build --progress plain -t spike-oci -f /tmp/b4/Dockerfile.oci --build-context base=oci-layout:///tmp/oci /tmp/b4 >/tmp/row.out 2>&1
echo "ROW4-oci rc=$? $(nerdctl run --rm spike-oci 2>&1 | tr '\\n' +)"; tail -6 /tmp/row.out | sed 's/^/    | /'
echo "buildkitd log:"; tail -8 /tmp/buildkitd.log | cut -c1-300 | sed 's/^/    > /'
"""


def test_c2_nerdctl_build(lux, runners, hosts):
    spike(lux, runners, hosts, "c2-nerdctl-build", NERDCTL, "export SKIP_ROWS='1 2 3 4 5 5b 6 7'\n", NERDCTL_BUILD)
