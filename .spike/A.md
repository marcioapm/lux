# Option A: dockerd with a real cgroup driver inside a nested Run

Run on vibes, worktree off origin/main a7b304a3 (with #113 delegation), via the
e2e harness: `tests/suites/test_spike_docker_limits.py` (tests a0–a3), image
`tests/images/spike` (alpine 3.24.2: docker 29.8.2, buildx 0.34.1, compose
5.1.4, rootlesskit 2.3.5, runc 1.4.3; `moby/buildkit:v0.33.1` and alpine
loaded from tarballs, so nothing reaches a registry). Matrix script:
`tests/images/spike/matrix.sh`. Raw output: `.spike/raw/a*.log`.

Every Run: `sandbox.nestedContainers`, `resources: {cpus: 2, memory: 1Gi}`,
workload uid 1000, `CapEff 0`, host uid map 2147483647+ (not privileged), in
`0::/workload/main`.

```
cd tests && LUX_TEST_PG_PORT=55920 LUX_TEST_S3_PORT=59120 timeout --foreground 1500 \
  uv run python run_tests.py suites/test_spike_docker_limits.py -k "a0 or a1 or a2 or a3" -s </dev/null
```

## Results

| Row | a0 stock `dockerd-rootless` | a1 cgroupfs, not rootless | a2 a1 + read-only sysfs runtime | **a3 a2 + cgroup re-root** |
|---|---|---|---|---|
| 1 `-m 128m`, OOM | FAIL `memory.max=max`, 256M alloc succeeds, OOMKilled=false exit 0 | PASS 134217728, `true:137` | PASS | **PASS** 134217728, OOMKilled=true, 137 |
| 2 `--cpus .5` / `--pids-limit 64` | FAIL `cpu=max 100000 pids=max`, cgroup `0::/` | PASS | PASS | **PASS** `50000 100000`, 64 |
| 3 `-p 8080:80` from the Run | PASS | PASS | PASS | **PASS** |
| 4 `docker build` (BuildKit and classic) | PASS | PASS | PASS | **PASS** |
| 5 buildx docker-container, `network=host`, `image=moby/buildkit:v0.33.1`, `--load` | PASS | FAIL (a) | FAIL (a) | **PASS** |
| 5b same + `--driver-opt cgroup-parent=/workload/buildx` | – | FAIL (b) | PASS | (not needed) |
| 6 compose, `deploy.resources.limits.memory: 64M` | FAIL `memory.max=max` (service-to-service works) | PASS 67108864 | PASS | **PASS** |
| 7 `-m 4g` in a 1 GiB Run | PASS (137; bounded by the Run) | PASS | PASS | **PASS** 137, Run `memory.max=1022312448` |

Exact errors:

- (a) `ERROR: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: unable to apply cgroup configuration: mkdir /sys/fs/cgroup/docker: permission denied`
  — `docker inspect`: `cgroupparent=/docker/buildx privileged=true net=host`.
- (b) `... runc create failed: unable to start container process: error during container init: error mounting "sysfs" to rootfs at "/sys": mount src=sysfs, dst=/sys, dstFd=/proc/thread-self/fd/14, flags=MS_NOSUID|MS_NODEV|MS_NOEXEC: operation not permitted`
- a1 first attempt, `dockerd-rootless --rootless=false`: `rootless mode needs to be enabled for running with RootlessKit` (daemon refuses).

Isolation probes (a1 vs a2, `PROBE_PRIV=1`, plain `docker run` of alpine):

| `docker run` options | a1 (runc) | a2 (runc-ro-sysfs) |
|---|---|---|
| `--network host` | ok | ok |
| `--privileged` | FAIL (b) | ok |
| `--privileged --network host` | FAIL (b) | ok |
| `--privileged --network host --cgroup-parent /workload/probe` | FAIL (b) | ok |

So the docker-container driver's failure is **neither network=host nor the
BuildKit image**: it is two independent things buildx always asks for.

## Root cause

1. **Why stock rootless Docker ignores limits.** `daemon/daemon_unix.go`:
   `cgroupDriver()` returns `none` whenever `cfg.Rootless` and not
   `UsingSystemd`; `withRootless` → `specconv.ToRootless(spec, nil)` then sets
   `spec.Linux.Resources = nil` and `CgroupsPath = ""`. The systemd path needs
   a user systemd (it reads `/sys/fs/cgroup/user.slice/user-<euid>.slice/
   cgroup.controllers` and talks to systemd over D-Bus); there is none in a Run.
   `cfg.Rootless` is set by `rootless.RunningWithRootlessKit()`, which is only
   `os.Getenv("ROOTLESSKIT_STATE_DIR") != ""`, and `--rootless=false` with that
   variable set is refused at startup.
2. **The way round, no patch:** let `dockerd-rootless` create the RootlessKit
   user+mount+net namespaces as usual, but exec dockerd through a 3-line
   wrapper (`DOCKERD=dockerd-userns`) that unsets `ROOTLESSKIT_*`. dockerd then
   runs as "rootful-in-rootless" (its own code path for dockerd inside a user
   namespace: `withRootfulInRootless`, only an OOM score fix-up), uses the
   `cgroupfs` driver, and with `--cgroup-parent=/workload` puts each container
   at `/workload/<id>`, i.e. in the subtree #113 delegates. Not being
   "rootless" it no longer knows RootlessKit's detached netns or port API, so:
   `DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false`, pasta with
   `--port-driver=implicit` (dockerd's own docker-proxy binds in RootlessKit's
   netns, pasta forwards it — row 3 passes), and data-root/exec-root/socket/
   pidfile given explicitly. Uid 0 inside is RootlessKit's userns root, which
   maps to the workload user (uid 1000) of the Run: nothing gains host
   privilege, and every write to the cgroupfs is checked against the files
   the shim chowned to that user.
3. **buildx (a): cgroup parent.** buildx's docker-container driver
   (`driver/docker-container/driver.go`, `create`) sets
   `hc.CgroupParent = "/docker/buildx"` whenever `docker info` reports
   `CgroupDriver == "cgroupfs"`. In the Run's cgroupns `/docker` is a sibling of
   `/workload` at the Run's root, owned by the Run's root (host-owned), so
   `mkdir /sys/fs/cgroup/docker` is EACCES. This is exactly what O3 hit: it
   only appears once dockerd has a cgroup driver (stock rootless reports `none`,
   so buildx leaves the parent empty and row 5 passes in a0 — without limits).
   Fix without touching buildx: either the user passes
   `--driver-opt cgroup-parent=/workload/buildx` (5b), or the runtime re-roots
   any cgroupsPath outside `/workload` below it (a3; transparent to
   docker/setup-buildx-action, which passes no cgroup-parent).
4. **buildx (b): `--privileged` sysfs.** buildx always creates BuildKit with
   `Privileged: true`. Privileged drops the read-only option from the `/sys`
   mount; the Run's own `/sys` is mounted `ro,nosuid,nodev,noexec`, and in a
   user namespace the kernel only allows a new sysfs mount that is no less
   restricted than the one visible (mount-lock), so `mount sysfs rw` →
   EPERM. Stock rootless dockerd passes row 5 only because buildx asks for
   `network=host` and, in rootless mode with a detached netns,
   `specconv.toRootless` → `removeSysfs` drops `/sys` (keeping
   `/sys/fs/cgroup`) for host-netns containers; a plain `--privileged`
   container is refused there too (docs/podman.md). Non-rootless dockerd does
   not apply `specconv` at all. Fix: the OCI runtime marks
   every sysfs mount `ro` before `runc create` (a2/a3). Privileged inside here
   is still the RootlessKit userns: no host capabilities.

The image-side pieces, all in `tests/images/spike`:

- `dockerd-userns`: unset `ROOTLESSKIT_*`, `exec dockerd "$@"`.
- `runc-ro-sysfs`: jq-edits `config.json` on `create`/`run` (sysfs → `ro`;
  with `RO_SYSFS_REROOT=/workload`, any `linux.cgroupsPath` outside it is
  prefixed), then `exec runc`. Registered with `--add-runtime` and
  `--default-runtime`, named by a path wrapper because containerd's shim does
  not pass dockerd's environment to the runtime.
- dockerd flags: `--exec-opt native.cgroupdriver=cgroupfs --cgroup-parent=/workload
  --data-root ~/.local/share/docker --exec-root $XDG_RUNTIME_DIR/docker
  -H unix://$XDG_RUNTIME_DIR/docker.sock --pidfile ...`.

BuildKit itself in a3: `docker exec buildx_buildkit_spk0 cat /proc/1/cgroup` →
`0::/init`, `memory.max` `max` at its cgroupns root, i.e. its own cgroup
under `/workload/docker/buildx/<id>`, still inside the Run's limit. (The step
cgroup probe in the Dockerfile printed unexpanded, so RUN-step cgroups were
not captured.)

## Risks / caveats

- Unsupported configuration: dockerd in "rootful-in-rootless" mode under
  RootlessKit without telling it. Docker could start refusing it (e.g. also
  detecting RootlessKit by its API socket or parent euid). Worth a Docker
  version pin and an e2e guard.
- The runc wrapper is a config.json rewrite on every create. jq dependency;
  a Go helper would be sturdier. A sysfs that is read-only in a privileged
  container is a behaviour change for workloads that write `/sys` (none of the
  matrix did; inside a userns they mostly cannot anyway).
- Not rootless means dockerd sets up its iptables in RootlessKit's netns
  itself (worked) and uses docker-proxy for `-p`; no detached netns, so
  `docker build`/pull traffic goes through pasta (performance not measured).
- `--privileged` inner containers now start (stock rootless Docker refused
  them in docs/podman.md). Bound: the RootlessKit userns inside the Run's
  userns, the Run's seccomp/AppArmor, no host caps; still worth a review.
- Not measured: `docker buildx create --driver-opt memory=...` enforcement,
  `docker stats`, `docker update`, cgroup.max.descendants (1024) under many
  containers, resume/restart of the engine with leftover `/workload/<id>`
  cgroups.
