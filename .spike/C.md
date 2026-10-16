# Option C: nerdctl + rootless containerd + BuildKit in a nested Run

Same harness/image (`tests/suites/test_spike_docker_limits.py` tests c1, c2;
`tests/images/spike`: nerdctl 2.2.1, containerd 2.3.6, rootlesskit 2.3.5,
Alpine's buildkitd 0.29.0, `buildctl` copied from `moby/buildkit:v0.33.1`,
nerdctl's own `containerd-rootless.sh` from v2.2.1 vendored as
`tests/images/spike/containerd-rootless.sh`). Raw: `.spike/raw/c*.log`.

Setup (all as the workload user, no privileges; see `NERDCTL` in the suite):

- `containerd-rootless.sh` (RootlessKit, slirp4netns, builtin port driver,
  detached netns) with `XDG_DATA_HOME` on the runner's engine volume
  (`~/.local/share/docker/c`, for kernel overlay).
- `~/.config/nerdctl/nerdctl.toml`: `cgroup_manager = "cgroupfs"`. nerdctl's
  default without systemd in rootless mode is `none`
  (`pkg/defaults/cgroup_linux.go`), which discards `-m/--cpus/--pids-limit`
  with a warning, the same failure as stock rootless Docker.
- containerd's default cgroupsPath is `/<namespace>/<id>` (`/default/...`),
  outside `/workload`; `runc` in containerd's PATH is the A3 wrapper
  (`runc-ro-sysfs` with `RO_SYSFS_REROOT=/workload`), which re-roots it.
  (`--cgroup-parent=/workload` per command would also do it; nerdctl.toml
  has no default cgroup parent.)
- buildkitd with the containerd worker (`--containerd-worker-rootless`),
  entered into RootlessKit's namespaces with `nsenter -U -m -n -t <child_pid>`,
  listening on `$XDG_RUNTIME_DIR/buildkit-default/buildkitd.sock` where
  nerdctl looks.
- `docker` → symlink to `nerdctl`, so `matrix.sh` runs the same commands.

Run not privileged (CapEff 0, host uid 2147483647+, `0::/workload/main`).

## Results (c1: full matrix with `docker`=nerdctl; c2: build rows the nerdctl way)

| Row | Result | Detail |
|---|---|---|
| 1 `-m 128m`, OOM | **PASS on enforcement, FAIL on the Docker contract** | inner `memory.max=134217728`, alloc killed, exit 137. `nerdctl inspect --format '{{.State.OOMKilled}}'`: `template parsing error: ... map has no entry for key "OOMKilled"`; `.State` = `{"Status":"exited",...,"ExitCode":137,"Error":""}`: no OOMKilled field. |
| 2 `--cpus .5` / `--pids-limit 64` | **PASS** | `50000 100000`, 64 |
| 3 `-p 8080:80` from the Run | **PASS** | |
| 4 `docker build` | FAIL as written; PASS the nerdctl way | `build -q`: `level=fatal msg="no image was built"` (from BuildKit failing to resolve `FROM docker.io/library/alpine:3.24.2` from the registry: standalone BuildKit does not see containerd's image store for metadata; the harness is offline). With an OCI-layout base (`--build-context base=oci-layout:///tmp/oci`), the build, load and run work (`one+`). `oci-layout://...@sha256:` (works in buildx) fails: `open /tmp/oci@sha256:.../index.json: no such file or directory`. |
| 5 `docker buildx ...` | **FAIL (not available)** | `level=fatal msg="unknown flag: --name"`: nerdctl has no buildx; no docker-container driver. |
| 6 compose, `limits.memory: 64M` | **PASS** | `nerdctl compose`: 67108864, services resolve each other |
| 7 `-m 4g` in a 1 GiB Run | **PASS** | 137; containerd alive; Run `memory.max=1022312448` |

Other incompatibilities seen: `docker version --format '{{.Server.Version}}'`
→ `can't evaluate field Version in type *dockercompat.ServerVersion`;
`nerdctl info` warns `unable to determine buildctl version` until `buildctl`
is installed (Alpine's `buildkit` package ships only `buildkitd`).

## Root cause / reasoning

- Limits work for the same reason as A: a real cgroup manager writing under
  the delegated `/workload`. Out of the box both nerdctl (`none`) and
  containerd (`/default/<id>`) miss it; two config pieces fix it.
- The CLI is not Docker. CI here is GitHub Actions jobs written for Docker:
  `docker/setup-buildx-action` and `docker/build-push-action` need buildx
  and a Docker API; `docker inspect` templates and `docker version`/`info`
  fields differ; `docker compose` is `nerdctl compose` (a separate
  implementation, which passed the 2-service case). A `docker` symlink is
  not a drop-in.
- Builds need standalone buildkitd management (start it in RootlessKit's
  namespaces, and its own registry resolution for `FROM`): no
  `--driver docker-container`, so jobs that pin `moby/buildkit:v0.33.1` via
  buildx cannot run unchanged.

## Verdict

Viable for enforcement, not for running lux-gha/abs jobs unchanged: buildx
is missing entirely and the Docker CLI surface differs. Dead as the main
path; noted as fallback only.
