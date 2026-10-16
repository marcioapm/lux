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
  container's mount ns are not "locked" for the container; when an inner engine copies the mount ns into
  its own user ns, they become locked there, which is what `mnt_already_visible` needs.

