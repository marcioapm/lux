# Spike summary: enforced per-container limits for Docker-CLI workloads in a nested Run

Details: `.spike/A.md`, `.spike/B.md`, `.spike/C.md`; raw Run output in
`.spike/raw/`. Probe suite `tests/suites/test_spike_docker_limits.py`, image
`tests/images/spike/`, matrix `tests/images/spike/matrix.sh`. All Runs:
`sandbox.nestedContainers`, 2 CPUs / 1 GiB, not privileged (CapEff 0, host
uid 2147483647+, workload in `0::/workload/main`), on origin/main a7b304a3.

## Matrix

| Row | A0 stock rootless dockerd | **A3 dockerd, cgroupfs, + runtime wrapper** | B Podman Docker API | C nerdctl |
|---|---|---|---|---|
| 1 `-m` + OOMKilled/137 | FAIL (`max`) | **PASS** | PASS | enforced; no `.State.OOMKilled` |
| 2 `--cpus` / `--pids-limit` | FAIL | **PASS** | PASS | PASS |
| 3 `-p` | PASS | **PASS** | PASS | PASS |
| 4 `docker build` | PASS | **PASS** | FAIL (BuildKit path); classic PASS | nerdctl-only syntax |
| 5 buildx docker-container, network=host, v0.33.1 | PASS | **PASS** | FAIL; PASS with `cgroup-parent` opt | no buildx |
| 6 compose `limits.memory` | FAIL | **PASS** | PASS | PASS (`nerdctl compose`) |
| 7 bounded by the Run | PASS | **PASS** | FAIL once: engine OOM-killed | PASS |

## Ranked recommendation

1. **A: dockerd with the cgroupfs driver in "rootful-in-rootless" mode
   (A3).** The only option that passes all seven rows with the Docker CLI,
   buildx and compose unchanged. It needs:
   - `dockerd-rootless` as today, with `DOCKERD=<wrapper>` that unsets
     `ROOTLESSKIT_*` before `exec dockerd` (dockerd's rootless mode, which
     forces cgroup driver `none`, keys only on `ROOTLESSKIT_STATE_DIR`),
     and `DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false`, pasta,
     `--port-driver=implicit`;
   - dockerd flags `--exec-opt native.cgroupdriver=cgroupfs
     --cgroup-parent=/workload` plus explicit data-root/exec-root/host/pidfile;
   - a default OCI runtime wrapping runc that (1) makes sysfs mounts `ro`
     (buildx always creates BuildKit `--privileged`, whose rw sysfs mount
     the kernel refuses under the Run's read-only `/sys`: `error mounting
     "sysfs" ... operation not permitted`) and (2) moves any cgroupsPath
     outside `/workload` below it (buildx forces `/docker/buildx` when the
     driver is cgroupfs: `mkdir /sys/fs/cgroup/docker: permission denied`;
     this was O3's buildx failure).
2. **B: Podman's Docker API.** Limits, OOM, ports and compose work with no
   unsupported config. Builds break by default: plain `docker build` becomes
   a docker-container buildx builder with no `--load`, and buildx needs
   `--driver-opt cgroup-parent=/workload/...` in every job (or the same
   runtime wrapper via containers.conf). O1's port failure did not reproduce
   (most likely the passt AppArmor issue #115 fixed). One Run-level OOM
   killed the Podman service itself.
3. **C: nerdctl.** Enforces limits once configured, but has no buildx, a
   different `inspect`/`version` surface and its own compose. Not a drop-in
   for lux-gha/abs jobs.

## What a real lux change would look like

**Image-only for the engine, plus docs; no runner sandbox or shim change.**
#113's delegation already provides what all three options need: `/workload`
writable by the workload user with `+cpu +memory +pids +io` and
`cgroup.max.descendants 1024`. Every A3 piece runs as the workload user
inside the Run:

- Ship it as a small supported helper in images that want Docker (e.g. a
  `lux-dockerd` script, or a `docker-limits` mode documented in
  `docs/podman.md` Nested containers and used by `tests/images/docker`),
  wrapping `dockerd-rootless` with the env/flags above.
- Replace the jq/sh runtime wrapper with a small static Go binary (an OCI
  runtime shim that edits `config.json` then execs runc), testable in unit
  tests, possibly shipped from `/.lux/bin` like lux-shim if lux wants to own
  it across images. Doing it in lux-shim would make it a lux-provided
  runtime, still not a runner sandbox change.
- e2e: fold rows 1, 2, 5 and 6 into `tests/suites/test_nested_docker.py`
  (asserting values, as `test_nested.py` does for Podman), and replace the
  "Docker inside a Run does not enforce per-container limits" paragraph in
  `docs/podman.md`.
- Optional runner tweak only if wanted: none found necessary. A Docker
  default `cgroup-parent` of `/workload` relies on the shim's layout name;
  that name becomes an interface and should be documented as such.

## Risks

- **Unsupported Docker configuration.** dockerd is told it is not rootless
  while it runs under RootlessKit; Docker could tighten detection (the
  `--rootless=false` refusal already exists). Pin Docker in images, keep an
  e2e guard on every bump.
- **Privileged inner containers become possible** (BuildKit needs it).
  They are privileged only in RootlessKit's user namespace inside the Run's:
  no host capabilities, the Run's seccomp/AppArmor and cgroup limits still
  apply (row 7). Needs a security review against `docs/security.md`; stock
  rootless Docker refused `--privileged` until now.
- **Read-only sysfs in privileged containers** differs from Docker's
  behaviour; anything writing `/sys` inside a build breaks (not seen).
- **Networking**: no detached netns, so pulls/builds go through pasta in the
  daemon's netns; docker-proxy instead of RootlessKit's port API. Throughput
  not measured. The Run's egress applies (same netns chain as today), not
  re-verified for A3.
- **Run-level OOM**: an inner container that is allowed more than the Run
  can make the kernel kill the engine (seen with Podman once). Applies to
  every option; recommend docs telling users to keep `-m` within the Run's
  size, and measure dockerd/containerd `oom_score_adj` handling.
- `cgroup.max.descendants=1024` bounds containers + BuildKit step cgroups;
  large compose/matrix builds not measured.

## Open questions

- Will Docker accept an upstream option for cgroupfs under rootless without
  systemd (would retire the env-unset trick)? Out of scope here (no patches).
- Is `apparmor=unconfined` on Ubuntu hosts (docs/podman.md) needed for A3
  as for stock rootless Docker? Measured only on vibes (the harness's
  Podman-in-Docker hosts), not on an Ubuntu EC2 host.
- Does `docker/setup-buildx-action` with `driver-opts: network=host` and
  `image=moby/buildkit:v0.33.1` behave identically to row 5 (it also passes
  `--buildkitd-flags`, and may set `cgroup-parent` from inputs)? Row 5 uses
  the same CLI flags, not the action.
- BuildKit RUN-step cgroups under A3: BuildKit itself sits in
  `/workload/docker/buildx/<id>` (its cgroupns shows `0::/init`), inside the
  Run; per-step placement not captured.
- `docker stats`, `docker update -m`, and engine restart with stale
  `/workload/<id>` cgroups after a resume: not tested.
