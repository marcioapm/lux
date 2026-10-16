# Spike: LXCFS so Lux Runs (and their inner containers) see the Run's CPU/memory

Branch `lux-lxcfs-spike` off `origin/main` ced1e4c5. Spike only, no PR.
Raw output of every step: `.spike/out/<step>.log` (written by the suite).

## Setup

- **Where LXCFS runs:** inside the e2e harness's simulated host (host-a), never on vibes.
  Spike host image `tests/images/lxcfs-host` = `quay.io/podman/stable:v5.8.7` (Fedora 44)
  + `dnf install lxcfs` → **lxcfs 6.0.6-1.fc44, fuse3-libs 3.18.2**. Kernel (vibes) 7.0.0-1008-aws.
  The simulated host is already `--privileged`, so `/dev/fuse` was there; no extra harness privileges needed.
- `lxcfs --help` (6.0.6): `-l/--enable-loadavg`, `--enable-cfs` ("CPU virtualization via CPU
  shares"; in practice it reads `cpu.max` quota), `-u/--disable-swap`, `--enable-pidfd`, `--enable-cgroup`,
  `--runtime-dir`, `-p pidfile`. Started as `lxcfs --enable-cfs -l /var/lib/lxcfs`.
- **Runner change (spike only, env-gated):** `internal/runner/spike_lxcfs.go`, appended in
  `createArgs()` (`internal/runner/placement.go`):
  - `LUX_SPIKE_LXCFS=/var/lib/lxcfs` → `-v <dir>/proc/{meminfo,cpuinfo,stat,uptime,loadavg,diskstats,swaps}:/proc/<f>:ro`
    and `-v <dir>/sys/devices/system/cpu/online:...:ro` (`LUX_SPIKE_LXCFS_SYS=cpu` binds the whole
    `cpu` dir instead, `none` nothing).
  - `LUX_SPIKE_HIDDEN_PROC=1` / `LUX_SPIKE_HIDDEN_SYS=1` → `--annotation io.lux.spike.hidden=proc[,sys]`
    for the OCI hook (step 3).
- **Shim change (spike only):** `internal/shim/cgroup.go` copies the Run's `cpu.max` to `lux.shim/`
  too in delegated Runs (see step 2, "cpu.max is not walked up").
- Workload image `tests/images/lxcfs` (alpine 3.24.2/musl): podman + rootless docker + python3 +
  node + procps + `/opt/probe.sh` (prints `P:<tag>:<key>=<value>`).
- Suite `tests/suites/test_spike_lxcfs.py`, one test per step; harness wired in
  `tests/run_tests.py`/`tests/build.py`/`tests/env.py` (`env.extra["host_image"]`).
- Command per step:
  `cd tests && LUX_TEST_LOG_ROOT=$PWD/../.spike/logs LUX_TEST_PG_PORT=55920 LUX_TEST_S3_PORT=59120 timeout --foreground 1500 uv run python run_tests.py suites/test_spike_lxcfs.py -k stepN -s </dev/null`

Simulated host (step0): 32 CPUs, MemTotal 64418888 kB, its own cgroup `/` (no limits).

### Harness finding: LXCFS takes its cgroup hierarchy from pid 1's cgroup

On first start inside the simulated host, every LXCFS file showed **host values, everywhere**. LXCFS 6.0.6
(`cg_unified_init`, src/cgroups/cgfsng.c) takes the unified hierarchy's controller list from
`/sys/fs/cgroup/<pid 1's cgroup>/cgroup.controllers`. In the sim host, pid 1 (`sleep`) is in `/` until
Podman moves it to `/init`; until then LXCFS logs `hierarchies: 0: fd: 8:` (no controllers) and every
file falls back to the host's (`read_file_fuse("/proc/meminfo")`), **silently**. The suite now moves pid 1
into `/init` and enables all controllers in `/` before starting LXCFS → `0: fd: 8: cpuset,cpu,io,memory,…`.
On a real systemd host pid 1 is in `/init.scope`, which LXCFS prunes to `/`, so this is harness-only; but
**a production check must verify LXCFS is functional** (e.g. read `/var/lib/lxcfs/proc/meminfo` from a
limited cgroup), because the failure mode is silent host values.

## Step 1 — plain Run, overmounts only: WORKS

Run: `resources: {cpus: 2, memory: 1Gi}` → cgroup `memory.max=1022312448` (runner headroom
scaling), `cpu.max=200000 100000`, `cpuset.cpus.effective=0-31` (no cpuset).

| tool | expected | baseline (no LXCFS) | LXCFS overmounts |
|---|---|---|---|
| `/proc/meminfo` MemTotal | 998352 kB (=memory.max) | 64418888 kB | **998352 kB** ✅ |
| `/proc/meminfo` MemAvailable | ≤ limit | 25.4 GB | **~995000 kB** ✅ |
| `free` total | 998352 | 64418888 | **998352** ✅ |
| `/proc/cpuinfo` processors | 2 | 32 | **2** ✅ |
| `/proc/stat` cpuN lines | 2 | 32 | **2** ✅ |
| `/proc/uptime` | Run age | 7551147 s (host) | **0.14 s** ✅ (container age) |
| `/proc/loadavg` (`-l`) | Run's | 20.26 19.55 19.35 | **0.00 0.00 0.00** ✅ |
| `/proc/swaps` | no swap | 2 lines | **1 line (header)** ✅ |
| `/sys/devices/system/cpu/online` | 0-1 | 0-31 | **0-1** ✅ (both `online`-only and whole-`cpu`-dir binds) |
| `nproc` (busybox, musl) | 2 | 32 | **32** ❌ affinity mask |
| `getconf _NPROCESSORS_ONLN` (musl) | 2 | 32 | **32** ❌ musl uses sched_getaffinity |
| Python `os.cpu_count()` (musl) | 2 | 32 | **32** ❌ musl sysconf |
| Python `os.sched_getaffinity` / `process_cpu_count()` | – | 32 | 32 (affinity, by design) |
| Node `os.cpus().length` | 2 | 32 | **2** ✅ (reads /proc/cpuinfo) |
| Node `os.availableParallelism()` | 2 | **2** already | 2 (libuv reads cgroup `cpu.max`) |
| Node `os.totalmem()` | limit | 65964941312 | **1022312448** ✅ (libuv: cgroup memory.max) |

### Step 1b — glibc tools (Fedora sim host, glibc 2.43), process in a cgroup with `cpu.max=200000 100000`, LXCFS files bound in a private mount ns

| tool (glibc) | value |
|---|---|
| LXCFS `online` / `cpuinfo` | `0-1` / 2 |
| coreutils `nproc` | **2** — coreutils ≥9.x reads the cgroup v2 `cpu.max` quota itself (not LXCFS) |
| `nproc --all` | 32 |
| `getconf _NPROCESSORS_ONLN` | **2** ✅ follows `/sys/devices/system/cpu/online` |
| `getconf _NPROCESSORS_CONF` | 32 (reads `/sys/devices/system/cpu/` dir entries; only the whole-`cpu`-dir bind could change it) |
| Python `os.cpu_count()` | **2** ✅ (glibc `get_nprocs()` → `online`) |
| Python `len(os.sched_getaffinity(0))` | 32 (affinity) |

So on **glibc** images `os.cpu_count()`, `getconf`, `sysconf(_SC_NPROCESSORS_ONLN)` follow LXCFS's
`online`; on **musl** (Alpine) they do not (musl uses `sched_getaffinity`), nor does busybox `nproc`.
Only a real cpuset changes the affinity mask. Recorded, not fought.

Notes:
- LXCFS uses the cgroup of **the reader's pid-namespace init** (`lookup_initpid_in_store` →
  `get_pid_cgroup(initpid)`), not the reader's own. In a Lux Run that is the shim (pid 1) — fine for
  non-nested Runs (shim lives in the container's cgroup).
- Existing masks (`/proc/kcore`, `/proc/keys`, `/sys/firmware`, … as tmpfs; `/proc/sys` ro) are unchanged.

## Step 2 — nested Run, overmounts only: Run sees limits, inner engines FAIL (reproduced)

`sandbox.nestedContainers` Run (`unmask=ALL`, delegated cgroup) + the same LXCFS binds.

**Run itself** (after the shim fix below): MemTotal 998352 kB, cpuinfo 2, stat 2, online `0-1`,
uptime 0.14, loadavg 0, swaps header only; `nproc`/musl `os.cpu_count()` 32 (affinity); Node
`cpus()` 2, `totalmem` 1022312448. **`podman info`** in the Run: `CPUs=32 MemTotal=1022312448`;
**`docker info`**: `NCPU=32 MemTotal=1022312448` — both engines take MemTotal from `/proc/meminfo`
but the CPU count from the affinity mask (Go `runtime.NumCPU()`), so NCPU is never fixed by LXCFS.

**Inner containers — every one fails**, Podman (Alpine's podman runs runc) and rootless Docker:

```
Error: runc: runc create failed: unable to start container process: error during container init:
error mounting "proc" to rootfs at "/proc": mount src=proc, dst=/proc, dstFd=/proc/thread-self/fd/15,
flags=MS_NOSUID|MS_NODEV|MS_NOEXEC: operation not permitted: OCI permission denied
```
```
docker: Error response from daemon: failed to create task for container: failed to create shim task:
OCI runtime create failed: runc create failed: unable to start container process: error during container
init: error mounting "proc" to rootfs at "/proc": mount src=proc, ... operation not permitted
```

Baseline (same nested Run, no LXCFS binds): inner Podman runs, as before.

Root cause (kernel, `fs/namespace.c` `mount_too_revealing()` → `mnt_already_visible()`): a new procfs
inside a non-init user namespace is allowed only if a procfs of that type is already mounted in the
mount namespace **with every locked child mount sitting on an empty directory**. The LXCFS binds are
locked child mounts on *files* (`/proc/meminfo`…), so the Run's `/proc` no longer counts as fully
visible → `EPERM`. Same mechanism as the masks that `unmask=ALL` removes.

### Step 2 finding: in delegated (nested) Runs LXCFS saw host values until the shim was changed

First run of step 2 (`.spike/out/step2b_diag.log`): the nested Run read MemTotal 64418888 kB and 32 CPUs
through LXCFS. Diagnosis by placing reader processes in each cgroup on the sim host
(`tests/images/lxcfs-host/lxcfs-reader.sh`):

| reader's (pidns-init's) cgroup | memory.max / cpu.max there | LXCFS mem / cpus |
|---|---|---|
| `<run>/lux.shim` (where the shim, pid 1, lives) | max / max | 998352 kB / **32** |
| `<run>/workload/main` | 1022312448 / 200000 | 998352 kB / 2 |
| `<run>/workload/<engine child, no limit>` | max / max | 998352 kB / **32** |
| `/spike-top` (512 MiB, 3 CPUs) | | 524288 kB / 3 |

- **memory**: LXCFS walks up the cgroup tree for `memory.max` (`cgroup_walkup_to_root`), so any
  descendant of the Run's cgroup reports the Run's memory. ✅
- **cpu**: `read_cpu_cfs_param` reads `cpu.max` **only in the exact cgroup** (`cgfsng_get`, no walk-up;
  same in v7.0.0). The shim's `lux.shim/` has `cpu.max=max` → 32 CPUs.
- LXCFS resolves the cgroup of the reader's **pid-namespace init**, i.e. the shim for every process in
  the Run, whichever `workload/*` cgroup it is in.

Spike fix: the shim copies the Run's `cpu.max` into `lux.shim/cpu.max` too (it already copies
`cpu.max`/`memory.max`/`memory.swap.max` into `workload/main`). Harmless: `lux.shim/` is under the Run's
cgroup, so it caps nothing the Run's own limit doesn't. With it, the nested Run reads 2 CPUs.

## Step 3 — nested Run, overmounts + clean procfs/sysfs at `/dev/.lux/{proc,sys}`: INNER ENGINES WORK

### How the second procfs gets in

Options considered, cheapest first:
- `podman run --mount type=proc,destination=/dev/.lux/proc` → `Error: invalid filesystem type "proc"`
  (Podman 5.8.7). An OCI `mounts` entry of type `proc` would need a spec rewrite (an OCI runtime
  wrapper or `podman create` + edit `config.json`); crun then performs that mount itself, in order,
  inside the Run's user namespace — it would likely work (crun mounts `/proc` before the bind mounts
  and LXD-style ordering would be preserved) but needs a runtime wrapper. Not pursued in the time box.
- crun annotation: none for an extra procfs.
- **OCI hook (chosen):** `tests/images/lxcfs-host/lux-spike-hidden` + `hook.json.in`, stage `poststart`,
  matched by annotation `io.lux.spike.hidden=proc,sys` that the spike runner adds. Runs as host root:
  `nsenter -t <pid> -p -- mount -N /proc/<pid>/ns/mnt -t proc proc /dev/.lux/proc` (fork into the Run's
  pid namespace so the procfs is the Run's; switch into its mount ns just before `mount(2)`). The sysfs
  is mounted the same way with `-n` (net ns: sysfs's `/sys/class/net` is per-netns). Mounted from the
  init user namespace → no `mount_too_revealing` check; a fresh, **unmasked, unobstructed** instance.
  `poststart` is safe for Lux: the shim waits for the runner's handshake before it starts the workload.

Gotchas hit (each cost one iteration):
1. Podman 5.8.7 in the sim host reads hooks only from **`/usr/share/containers/oci/hooks.d`**
   (`--log-level=debug`: `reading hooks from /usr/share/containers/oci/hooks.d`); `/etc/containers/oci/hooks.d`
   is ignored when `hooks_dir` isn't set. Production must set `hooks_dir` or install there.
2. The hook can't `mkdir /dev/.lux` in the container: host root is unmapped in the Run's user namespace
   → `mkdir: … Value too large for defined data type` (EOVERFLOW). The runner adds empty mountpoints
   `--tmpfs /dev/.lux/proc:ro,size=4k,mode=0555` (and `/dev/.lux/sys`); the hook mounts over them.
3. **A hidden procfs alone is not enough** (`.spike/out/step3_hidden_procfs.log`, "hidden proc"): the inner
   runtime then gets past `/proc` and fails on sysfs, because `/sys/devices/system/cpu/online` is also
   an overmount:
   ```
   Error: runc: runc create failed: unable to start container process: error during container init:
   error mounting "sysfs" to rootfs at "/sys": mount src=sysfs, dst=/sys, dstFd=/proc/thread-self/fd/14,
   flags=MS_RDONLY|MS_NOSUID|MS_NODEV|MS_NOEXEC: operation not permitted: OCI permission denied
   ```
   Same rule (`mnt_already_visible` applies per fs type: proc *and* sysfs). Exactly why LXD mounts both
   `/dev/.lxc/proc` and `/dev/.lxc/sys`.

### Result matrix (nested Run 2 CPU / 1 GiB, LXCFS binds on `/proc/*` and `/sys/.../cpu/online`)

| config | Run sees limits | nested Podman `run` | rootless Docker `run` |
|---|---|---|---|
| no LXCFS (baseline) | no | ✅ | ✅ (existing suite) |
| overmounts only (step 2) | ✅ | ❌ `mounting "proc" … operation not permitted` | ❌ same |
| + hidden procfs | ✅ | ❌ `mounting "sysfs" … operation not permitted` | ❌ same |
| **+ hidden procfs + sysfs** | ✅ | ✅ (`-m 256m --cpus 1` too) | ✅ (and `--cpus/-m` accepted) |

Engines' own view in the Run: `podman info` CPUs=32 MemTotal=1022312448; `docker info`
NCPU=32 MemTotal=1022312448 (NCPU from the affinity mask; LXCFS can't change it).
Inner containers themselves see **host values** (fresh procfs → 64418888 kB, 32 CPUs, `online 0-31`) — step 4.
Side note: rootless Docker's inner `-m 256m --cpus 1` container shows `memory.max=max cpu.max=max` (rootless
dockerd with cgroupfs driver does not apply limits without systemd); Podman's does (268435456 / 100000).
Pre-existing, unrelated to LXCFS.

### What is visible at `/dev/.lux/*` vs `/proc`, `/sys`

| item | nested Run `/proc`,`/sys` (unmask=ALL) | nested Run hidden | **non-nested** Run `/proc`,`/sys` | non-nested Run hidden |
|---|---|---|---|---|
| procfs pid view | Run's pid ns | Run's pid ns (pid 1 = `lux-shim`, 5 pids) | Run's | Run's |
| `/proc/meminfo`, `cpuinfo` | LXCFS (Run values) | **host values** (raw procfs) | LXCFS | host values |
| `/proc/kcore`, `timer_list` | not readable (uid 1000) | not readable | masked | not readable |
| `/proc/keys`, `latency_stats` | readable | readable | masked (/dev/null) | readable |
| `/proc/sys` | rw mount, but `kernel/hostname` write refused (not owner) | same | ro | write refused |
| `/sys/firmware` | `acpi dmi efi` visible | visible | **masked** (empty tmpfs) | **visible** |
| `/sys/devices/system/cpu/online` | `0-1` (LXCFS) | `0-31` | `0-1` | `0-31` |
| `/sys/fs/cgroup` under hidden sysfs | – | empty dir (no cgroupfs submount) | – | empty |

- **Nested Runs: nothing new is exposed.** `unmask=ALL` already shows everything the hidden mounts
  show; the only difference is that the hidden copies carry the *real* `meminfo/cpuinfo/stat/online`,
  i.e. a workload that wants host totals can read them at `/dev/.lux/proc/meminfo` — an information
  disclosure LXCFS was never a boundary against (host totals are also derivable from
  `/sys/devices/system/node`, `/proc/zoneinfo`, etc.).
- **Non-nested Runs must not get the hidden mounts**: there they undo masks (`/sys/firmware`,
  `/proc/keys`, `/proc/latency_stats`, …). The real change gives them only to `nestedContainers` Runs —
  and only when LXCFS overmounts are present (without overmounts nested Runs don't need them).
- The workload is root-in-userns only via setuid `newuidmap`; with `CAP_SYS_ADMIN` in its user ns it could
  `umount /dev/.lux/proc` — that only breaks its own nested engines. The mounts made by host root inside the
  container's mount ns are not "locked" for the container (reasoning, not measured); when an inner engine copies the mount ns into
  its own user ns, they become locked there, which is what `mnt_already_visible` needs.


## Step 4 — inner containers see limits? Yes with binds, with caveats

Setup as step 3 (LXCFS overmounts + hidden procfs/sysfs). Inner containers get the **Run's** LXCFS files
(`/proc/meminfo` etc. in the Run are FUSE files from the host's LXCFS) bound over their own `/proc/*` and
`/sys/devices/system/cpu/online`. They are reachable through the bind because the FUSE mount lives in
the host's LXCFS; the read is served with the **reader's** pid (`fuse_get_context()->pid`, in the host's
pid ns) → LXCFS resolves the inner container's pid-ns init → the **inner container's own cgroup**.

| inner container | MemTotal | cpuinfo / stat / online | uptime | expected |
|---|---|---|---|---|
| Podman, no limit, `-v` binds | **998352 kB** (Run's: memory.max walk-up) | **32 / 32 / 0-31** ❌ | 0.12 s (inner's) | 1 GiB / 2 |
| Podman `-m 256m --cpus 1`, `-v` binds | **262144 kB** ✅ | **1 / 1 / 0** ✅ | 0.14 | 256 MiB / 1 |
| Podman, no limit, `containers.conf` `volumes=[…]` (no per-container flag) | 998352 kB ✅ | 32 ❌ | | |
| Podman `-m 256m --cpus 1`, `containers.conf` | 262144 kB ✅ | 1 ✅ | | |
| rootless Docker, no limit, `-v` binds | 998352 kB ✅ | 32 ❌ | 0.12 | |
| rootless Docker `-m 256m --cpus 1`, `-v` binds | 998352 kB (limit not applied, see below) | 32 | | |
| (step 3, no binds — fresh procfs) | 64418888 kB (host) | 32 | host | |

- **Podman default mounts work**: `~/.config/containers/containers.conf`
  `[containers] volumes = ["/proc/meminfo:/proc/meminfo:ro", …, "/sys/devices/system/cpu/online:…:ro"]`
  is applied to every `podman run` with no per-container flags; inner containers start fine (the inner
  runtime mounts its own procfs before the binds; the hidden procfs/sysfs in the Run satisfy the kernel).
  A Lux image or the runner could ship that file at `/etc/containers/containers.conf.d/` in the Run —
  but that is image-specific; a runner-mounted drop-in path is the generic option.
- **Memory walks up, CPU doesn't** (same root cause as step 2): an inner container without its own
  `cpu.max` reads `max` in its own cgroup and LXCFS reports the host's CPU count, not the Run's 2. Only
  fixable by (a) patching LXCFS to walk `cpu.max` up like `memory.max` (small, upstreamable: use
  `cgroup_walkup_to_root` in `read_cpu_cfs_param`), or (b) engines defaulting a `--cpus` per container.
- **Rootless Docker applies no `-m/--cpus`** in this Run (`memory.max=max`, `cpu.max=max` inside the
  inner container): rootless dockerd uses the cgroupfs driver without systemd here, so its limits are
  silently not set. Pre-existing and unrelated to LXCFS (the existing `test_inner_limits_are_enforced`
  covers Podman only); recorded as a separate finding.
- **Docker has no default-mounts setting** in `daemon.json`. Options, none tested here beyond `-v`:
  per-container `-v` / compose `volumes:` (works, tested); a `runtimes` entry pointing at an OCI
  wrapper that adds the binds to `config.json` before exec'ing runc (needs a wrapper binary in the image —
  same thing LXD/Incus avoid by doing it in the container manager); CDI (`--device vendor/lxcfs=all`,
  Docker ≥25 with the `cdi` feature) can inject mounts but still needs a per-container flag. Podman also
  honours CDI.
- `podman info`/`docker info` inside the Run: MemTotal follows LXCFS (1022312448), CPUs = 32 (affinity).

## Step 5 — failure mode (`.spike/out/step5_failure.log`)

A non-nested Run reads `/proc/meminfo` once a second (`timeout 5 awk … /proc/meminfo`), while on the host:

| event (host) | effect on the running Run's reads | host-side read of `/var/lib/lxcfs/proc/meminfo` |
|---|---|---|
| `SIGUSR1` (reload liblxcfs) | no gap, values unchanged | – |
| `SIGSTOP` (daemon hung) 8 s | **read blocks** — `R:4 rc=124` (killed by `timeout 5`); reads resume on `SIGCONT` | blocks (`rc=124` after 5.0 s) |
| `SIGKILL` (daemon dead) | **fails immediately**: `awk: /proc/meminfo: Socket not connected` (ENOTCONN), every read after | `Transport endpoint is not connected`; the `fuse.lxcfs` mount stays in `/proc/mounts` |
| new Run while dead | **placement fails**: `podman create … Error: statfs /var/lib/lxcfs/proc/uptime: transport endpoint is not connected` (runner `stage=container`) | |
| restart LXCFS | needs the dead mount removed first (`fusermount3 -u` / `umount -l`); without that `lxcfs` exits: `fuse: failed to access mountpoint /var/lib/lxcfs: Transport endpoint is not connected` | |
| after restart | **existing Run does not recover**: its binds point at the dead FUSE connection; it keeps getting ENOTCONN until the Run is restarted | new Runs get correct values (998352 kB, 2 CPUs) |

- A hung LXCFS = every `/proc/meminfo` / `cpuinfo` / `stat` reader in every Run on the host blocks
  uninterruptibly-ish (FUSE requests are killable, but `free`, `top`, JVMs, Go runtimes at start,
  `ps` reading `/proc/stat`… stall). A dead LXCFS = ENOTCONN on those files: many tools fail hard
  (`free`: `Socket not connected`), nested engines can't read meminfo, and **no new Run can start** on
  the host until LXCFS is back.
- LXCFS's own answer to this is the `liblxcfs.so` reload on `SIGUSR1` (upgrades without remount) and
  `Restart=on-failure` in its systemd unit. LXD/Incus additionally remount: on restart they re-bind
  the files into running containers. Lux would need the same (runner-driven: on LXCFS restart, for each
  live Run, `nsenter` + re-bind — or accept "Run sees ENOTCONN until restarted").

## Step 6 — cost (`.spike/out/step6_cost.log`)

1000 open+read+close in the nested Run, Python, loaded host (load ~20 on 32 CPUs, numbers are noisy):

| file | via LXCFS (`/proc/…`) | native (hidden procfs `/dev/.lux/proc/…`) | per read |
|---|---|---|---|
| meminfo | 609 ms | 30 ms | **~0.6 ms vs 0.03 ms (~20×)** |
| cpuinfo | 2205 ms | 631 ms | 2.2 ms vs 0.6 ms (~3.5×) |
| stat | 647 ms | 66 ms | 0.65 ms vs 0.07 ms (~10×) |

LXCFS RSS: 2.6 MB at start, 3.6 MB after the bench (threads 4 → 7). Negligible memory; the latency is
per-read FUSE round trips + a cgroup walk + `scm_init_pid` fork on a cache miss of the pid-ns init.
Matters only for tools that poll `/proc/stat` hot (top/htop at 1 Hz are fine; a profiler sampling
`/proc/stat` at kHz would notice).

## Recommendation

**Worth doing for plain Runs; for nested Runs only with the hidden procfs+sysfs, and with known gaps.**

What a real Lux change looks like:

1. **Host: an LXCFS daemon per runner host**, installed by the host provisioning (aiverse / the AMI),
   systemd unit `Restart=always`, mountpoint `/var/lib/lxcfs`, flags `--enable-cfs -l` (`-u` if hosts
   ever get swap and we don't want it shown). Distro package (Fedora 6.0.6 here; Ubuntu 24.04 ships a 5.0.x —
   check `--enable-cfs` there) or a pinned build.
2. **Runner**: on start, probe LXCFS is *functional* (read `/var/lib/lxcfs/proc/meminfo` from a child
   in a cgroup with a known `memory.max`; the failure mode is silent host values, see "Harness finding").
   If functional, `createArgs()` adds the `-v /var/lib/lxcfs/proc/{meminfo,cpuinfo,stat,uptime,loadavg,diskstats,swaps}:/proc/…:ro`
   and `…/sys/devices/system/cpu/online` binds; if not, it adds none and reports a host warning
   (never refuse to run). A host label (`lxcfs=true`) makes it observable.
3. **Nested Runs**: additionally an OCI hook (or the runner itself, after `podman start`, via
   `nsenter -p -n` + `mount -N`; the shim already waits for the runner's handshake) mounting a clean
   **procfs at `/dev/.lux/proc` and sysfs at `/dev/.lux/sys`**, with `--tmpfs` placeholders created by the
   runner. Both are required (proc alone → sysfs mount fails). Never for non-nested Runs (it undoes masks).
   Doing it from the runner rather than an OCI hook avoids depending on `hooks_dir` (Podman 5.8.7 here
   ignored `/etc/containers/oci/hooks.d`).
4. **Shim**: in delegated Runs, copy the Run's `cpu.max` into `lux.shim/` (LXCFS keys on the pid-ns init's
   cgroup and does not walk `cpu.max` up). One line; already in the spike.
5. **Inner containers (optional, per engine)**: Podman — ship a `containers.conf` drop-in with default
   `volumes` binding the Run's LXCFS files (works; inner `-m/--cpus` are then reported exactly). Docker —
   no default-mount knob; per-container `-v`/compose, or CDI, or an OCI runtime wrapper. An inner container
   *without* its own `--cpus` sees the host CPU count (memory is fine, it walks up) unless LXCFS is patched.

Not fixed by any of this (record, don't fight): `nproc` (busybox), musl `sysconf`/Python `os.cpu_count()`,
Go `runtime.NumCPU()` (→ `docker info` NCPU, `podman info` CPUs), Python `sched_getaffinity` — all use the
affinity mask; only a real cpuset would change them. glibc `sysconf(_SC_NPROCESSORS_ONLN)` / Python
`os.cpu_count()` on glibc, Node `os.cpus()` / `totalmem()`, `free`, JVM/.NET (read `/proc/meminfo` +
cgroups) *are* fixed. Note `LUX_CPUS` / `LUX_MEMORY_LIMIT` env already exist for tools that want truth.

### Risks

- **FUSE availability is now on the Run start path and every `/proc/meminfo` read.** Hung daemon →
  readers block; dead daemon → ENOTCONN in running Runs until they're restarted, and `podman create` fails
  for new Runs. Mitigations: runner health-checks LXCFS before each create (timeout read) and falls back to
  no binds; systemd `Restart=always`; re-bind into live Runs on restart (LXD does this), or document that a
  restart needs Run restarts.
- **Hidden procfs/sysfs**: for nested Runs it exposes nothing `unmask=ALL` doesn't already (measured table
  in step 3) except the *unvirtualised* host totals. For non-nested Runs it would undo masks
  (`/sys/firmware`, `/proc/keys`, `/proc/latency_stats` readable) — must stay gated on `nestedContainers`.
  It is mounted by host root via a hook/nsenter: path handling must be done with care (mountpoints made by
  the runner, not followed through workload-controlled symlinks; `mount -N` into the container's mnt ns
  resolves `/dev/.lux/*` in the container's root, where the workload can't replace `/dev` as it's a
  runner tmpfs — verify in the real change).
- LXCFS runs as host root and parses per-request data from `/proc/<pid>` of untrusted callers; it's the
  same exposure LXD/Incus hosts take, but a new privileged daemon on runner hosts.
- Silent fallback to host values when LXCFS misdetects the cgroup layout (seen in the harness).

### Open questions

- Ubuntu 24.04's lxcfs 5.0.x: same `cpu.max`/`memory.max` behaviour and `--enable-cfs`? (not tested here.)
- Patch LXCFS upstream so `read_cpu_cfs_param` walks up like memory (fixes unlimited inner containers and
  removes the shim tweak)? Or carry a pinned build.
- Do the inner-engine binds survive engine features we rely on (BuildKit `RUN` steps, compose)? Only
  `run` was tested. BuildKit's own containers get a fresh `/proc` → host values, harmless.
- Re-binding into live Runs after an LXCFS restart: runner-driven `nsenter` mount, or accept restart-only.
- `/proc/diskstats` and `loadavg` correctness under nested engines (only `MemTotal`/CPU were checked
  for inner containers).
- Is `cpuinfo` at 2.2 ms/read a problem for anything we run (e.g. JVM start reads it a few times: no)?
- Rootless Docker in nested Runs applies no `-m`/`--cpus` to its containers (cgroupfs driver without
  systemd): separate pre-existing finding, not LXCFS-related — worth its own ticket.
