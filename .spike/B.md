# Option B: Podman's Docker-compatible API in a nested Run

Same harness and image as A (`tests/suites/test_spike_docker_limits.py` tests
b1, b2; `tests/images/spike`, podman 5.8.8 + crun + pasta hard-linked as in
`tests/images/nested`). Raw output: `.spike/raw/b*.log`.

Engine: `podman system service --time=0 unix:///tmp/xdg/podman.sock`, the
docker CLI 29.8.2 / buildx 0.34.1 / compose 5.1.4 with
`DOCKER_HOST=unix:///tmp/xdg/podman.sock`. `docker info`: `cgroupdriver=cgroupfs`,
`security=[seccomp rootless]`. alpine and `moby/buildkit:v0.33.1` were
`podman load`ed first. Run not privileged (CapEff 0, host uid 2147483647+,
`0::/workload/main`), 2 CPUs / 1 GiB.

```
cd tests && LUX_TEST_PG_PORT=55920 LUX_TEST_S3_PORT=59120 timeout --foreground 1500 \
  uv run python run_tests.py suites/test_spike_docker_limits.py -k "b1 or b2" -s </dev/null
```

## Results

| Row | b1 | b2 (`buildx-stable-1` tag present, `cgroup-parent` given) |
|---|---|---|
| 1 `-m 128m`, OOM | **PASS** 134217728, OOMKilled=true, 137 | skipped |
| 2 `--cpus .5` / `--pids-limit 64` | **PASS** `50000 100000`, 64 | skipped |
| 3 `-p 8080:80` from the Run | **PASS** (also `podman run -p 8081:80` direct: PASS) | skipped |
| 4 `docker build` | **FAIL** BuildKit path (c); `DOCKER_BUILDKIT=0` classic PASS | FAIL (d); classic PASS |
| 5 buildx docker-container, `network=host`, v0.33.1, `--load` | **FAIL** (d) | FAIL (d) |
| 5b same + `--driver-opt cgroup-parent=/workload/buildx` | – | **PASS** |
| 6 compose, `limits.memory: 64M` | **PASS** 67108864, services resolve each other | skipped |
| 7 `-m 4g` in a 1 GiB Run | **FAIL** (e) | PASS 137, engine alive |

Exact errors:

- (c) `docker build` (BuildKit): `WARNING: No output specified with docker-container driver ...` then
  `ERROR: failed to build: Error response from daemon: lookup registry-1.docker.io on 10.89.0.1:53: server misbehaving`
  (podman log: `Trying to pull docker.io/moby/buildkit:buildx-stable-1...`).
- (d) `ERROR: Error response from daemon: crun: create `/sys/fs/cgroup/docker`: Permission denied: OCI permission denied`
  — `docker inspect`: `cgroupparent=/docker/buildx privileged=true net=host`.
- (e) `time="..." level=error msg="error waiting for container: unexpected EOF"`, rc 125;
  every docker call after it: `Cannot connect to the Docker daemon at unix:///tmp/xdg/podman.sock`.

Isolation probes (b2):

| `docker run` against Podman | result |
|---|---|
| `--network host` | ok |
| `--privileged` | `unable to upgrade to tcp, received 500` |
| `--privileged --network host` | ok (`/sys` ro) |
| `--privileged --network host --cgroup-parent /workload/probe` | ok |

## Root cause

- **Ports (O1's failure) is not Podman's**: with #115's pasta hard-link,
  `-p` through the Docker API works, and so does podman's own `-p`. The
  earlier harness failure was most likely the `passt` AppArmor profile that
  #115 fixed (the old image had the pasta→passt symlink); not re-established.
- **`docker build` with BuildKit**: Podman does not serve Moby's `/grpc`
  BuildKit session endpoint, so the docker CLI's default builder for this
  endpoint is a buildx **docker-container** builder of
  `moby/buildkit:buildx-stable-1` with no output (`--load` not implied).
  Offline it first fails to pull; with the image present (b2) it fails like
  row 5. Even when it runs, the result stays in BuildKit's cache unless
  `--load` is passed, so `docker build -t x . && docker run x` does not work.
  Classic builds (`DOCKER_BUILDKIT=0`, Podman runs them with Buildah) work.
- **buildx docker-container**: same cause as A (a). Podman reports
  `CgroupDriver=cgroupfs`, so buildx sets `CgroupParent=/docker/buildx`, and
  crun cannot create `/docker` at the Run's cgroupns root (host-owned). With
  `--driver-opt cgroup-parent=/workload/buildx` it builds and `--load`s
  (BuildKit at `0::/init` in its own cgroupns, inside the Run). Podman, unlike
  dockerd (A), mounts `/sys` correctly for a privileged host-network
  container, so the read-only-sysfs workaround is not needed for buildx; a
  privileged container on its own netns still fails (500 above).
- **Row 7 / engine survival**: in b1, the inner container given `-m 4g` drove
  the **Run's** memory.max; the Run-level OOM kill (Run root
  `memory.events: oom 1 oom_kill 1`) took out `podman system service` (or its
  conmon), not just the container. In b2 the same row killed only the
  container. The kernel picks the victim by badness across the whole Run,
  and the service process is a candidate (its RSS was not measured).
  dockerd survived the same row in a1 and a2, whose probes ran after it
  (one sample each). Mitigation would be a lower `oom_score_adj` for the
  service, which a non-root workload cannot set below its inherited value,
  or keeping inner containers' `-m` within the Run's memory. Not proven
  either way with two samples.

## Docker-CLI compatibility gaps found

- `docker build` default path: fails, see above (needs `DOCKER_BUILDKIT=0`,
  or a pre-created buildx builder with `cgroup-parent` and `--load`).
- `docker buildx create --driver docker-container` without `cgroup-parent`
  fails; docker/setup-buildx-action users would need `driver-opts:
  cgroup-parent=/workload/buildx` (or a runtime wrapper as in A3, via
  containers.conf `runtime`).
- `docker info` reports Podman's version (5.8.8), `docker version` API
  differences, no `/grpc`, no `docker system df -v` parity — not audited.

## Verdict

Limits, OOM, ports and compose work out of the box; it needs no
unsupported dockerd config. But the build story — the main CI workload — is
broken by default in two places (BuildKit `docker build`, buildx cgroup
parent) and only fixable by changing every job or wrapping the runtime.
