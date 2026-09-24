# Operations

## luxd

`luxd` is stateless. Postgres holds state and S3 holds bytes, so you can
run several instances behind a load balancer. Each runner holds one
WebSocket to one instance.

```bash
luxd migrate      # once per upgrade, as the database owner
luxd serve        # as many as you like
```

### Building

`make build` builds the operator console (`console/`, with
[Bun](https://bun.sh)) and then the binaries in `bin/`; luxd embeds the
console. A luxd built with plain `go build` and no console build serves, at
`/console/`, a page saying how to build it.

### Postgres

Postgres 13 or later (tested on 18); no extensions. `luxd migrate` runs as
the database owner, who must be able to create roles: it creates `lux_app`
(no SUPERUSER, no BYPASSRLS: row-level security is the tenant boundary),
sets its password to `LUX_APP_PASSWORD` (default `lux_app`: set your own),
and grants it every table, new ones included, on each run. `luxd serve`
connects as `lux_app`; `luxd admin` works with either DSN.

Each luxd also holds one long-lived connection, outside its pool and made
with the same DSN, listening for Run events (`LISTEN lux_events`), which
is what makes the console and `lux logs -f` live. LISTEN does not work
through PgBouncer in transaction or statement mode: point luxd at Postgres
directly, or at a session-mode pool. If it cannot listen, updates still
arrive, a few seconds late.

### Upgrading

1. Run `luxd migrate` with the owner's DSN. It applies what is new and is
   safe to run again; running luxds keep working meanwhile.
2. Restart (or roll) every `luxd serve` onto the new binary.

Migrate first: a luxd newer than its schema does not check it, and fails
requests that touch what is missing (luxds older than the schema keep
working). Configuration through the environment alone keeps working: a
configuration file is optional.

### Configuration

luxd reads a TOML file: `--config FILE` (before or after the command),
else `LUX_CONFIG`, else `/etc/lux/luxd.toml` if it exists. Every setting
also has an environment variable, which overrides the file, so a secret can
stay out of it (`LUX_DATABASE_URL`, `LUX_S3_SECRET_KEY`). An unknown key or
a bad value stops luxd, naming it. luxd warns when others can read the
file: keep it `chmod 600`, or keep secrets in the environment.
[luxd.example.toml](luxd.example.toml) has every key with its default and
its variable; the table below lists them by variable.

| Variable | Default | |
| --- | --- | --- |
| `LUX_DATABASE_URL` | — | For `serve`, a DSN for the `lux_app` role; for `migrate`, the owner's. One file can serve both: set this variable per command. |
| `LUX_APP_PASSWORD` | `lux_app` | `migrate` sets `lux_app`'s password to it, each run. Set your own. |
| `LUX_LISTEN` | `127.0.0.1:7070` | Address to serve on. |
| `LUX_PUBLIC_URL` | — | The URL runners and clients use. |
| `LUX_RUNNER_URL` | = public_url | The URL runners dial, if different from `LUX_PUBLIC_URL` (a private IP such as `http://10.0.1.10:7070`, unreachable from outside the VPC). Only clients, the console and MCP use `LUX_PUBLIC_URL`. |
| `LUX_RUNNER_BIN_DIR` | `/usr/local/lib/lux/runner` | Runner binaries luxd serves and hashes (sha256) at startup, for self-update: `<dir>/linux-{arm64,amd64}/{lux-runner,lux-shim}`. A missing arch or file is simply not offered. |
| `LUX_S3_BUCKET` | — | Where snapshots, output and artifacts go. |
| `LUX_S3_ENDPOINT` | AWS | For MinIO and other S3-compatible stores (path-style). |
| `LUX_S3_PUBLIC_ENDPOINT` | = endpoint | The endpoint presigned URLs are signed for, if runners and clients reach S3 by another name. |
| `LUX_S3_REGION` | `us-east-1` | |
| `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY` | AWS chain | Only luxd holds S3 credentials. |
| `LUX_LEASE` | `30s` | A host that misses heartbeats this long is lost, along with its live placements. |
| `LUX_TICK` | `1s` | Scheduler and reaper interval. |
| `LUX_DEFAULT_CPUS`, `LUX_DEFAULT_MEMORY`, `LUX_DEFAULT_DISK`, `LUX_DEFAULT_PIDS` | `2`, `8Gi`, `20Gi`, `1024` | Resources a Run gets when its spec sets none. |
| `LUX_SCALE_DOWN_AFTER` | `10m` | How long a provisioned host stays idle before it is drained and terminated. |
| `LUX_LAUNCH_TIMEOUT` | `10m` | How long a launched host may take to register before it is terminated. |
| `LUX_EC2_ENDPOINT` | AWS | Overrides the EC2 endpoint (tests). |
| `LUX_SAMPLE_EVERY` | `10s` | How often the system is sampled for history ([Operators](operators.md#history)). |
| `LUX_HISTORY_RAW` | `48h` | How long raw samples (hosts and placements: one per heartbeat) are kept. |
| `LUX_HISTORY_MINUTES` | `720h` | How long minute rollups are kept. |
| `LUX_HISTORY_HOURS` | `9600h` | How long hour rollups are kept. |
| `LUX_DEBUG` | — | Debug logging. |
| `LUX_CONSOLE_AUTH` | `key` | How the console signs people in: `key` or `cloudflare-access` ([Operators](operators.md#signing-in)). |
| `LUX_CF_ACCESS_TEAM`, `LUX_CF_ACCESS_AUD` | — | For `cloudflare-access`: the Access team (`acme` or `acme.cloudflareaccess.com`) and the application's AUD tag. |
| `LUX_CONFIG` | `/etc/lux/luxd.toml` | The configuration file. |

luxd also serves the operator console at `/console/` ([Operators](operators.md#the-console)).

### Tenants, keys and quotas

```bash
luxd admin create-tenant --name acme [--max-runs N] [--max-hosts N] [--retention-days 30]
luxd admin create-key --tenant T --scopes read,run
luxd admin create-operator-key [--name N]       # every tenant: see docs/operators.md
luxd admin set-quota --tenant T [--max-runs N] [--max-hosts N] [--max-storage BYTES] [--retention-days N]
```

- `--max-runs`: Runs that are not stopped or finished. Checked when a Run
  is submitted or resumed.
- `--max-hosts`: the tenant's registered hosts. Checked when a host
  registers.
- `--max-storage`: bytes in snapshots, output and artifacts not yet
  deleted by retention. Checked when a Run is submitted or resumed.
- `--retention-days`: how long a finished Run's blobs are kept (default
  30). The Run and its events stay. Resuming a finished Run clears its
  finish time, so a live Run's snapshots are never deleted.

A request over quota gets HTTP 429, and the CLI exits with code 5.

## Where bytes live

Every placement ends with a **snapshot** of its state volumes, its
**output**, and its **artifacts**, all written to the host's disk. They
reach S3 in the background:

1. The runner uploads each blob with `PUT /runner/blobs/{id}`. luxd streams
   it into S3 and verifies its sha256 on the way. Runners never hold S3
   credentials.
2. The host keeps its local copy, so a resume there moves nothing. It
   deletes the copy when:
   - luxd tells it the Run now runs elsewhere (from S3), or
   - the copy is older than the runner's `--host-ttl` (default 24h).

   A copy is never deleted before its upload finished.
3. A runner downloads a snapshot through a presigned S3 URL, valid for 15
   minutes: it asks `GET /runner/blobs/{id}`, and luxd redirects it only
   for blobs of a Run placed on that host. Artifacts are downloaded through
   luxd, which decompresses them (blobs are stored zstd) and sends the file
   with its length and sha256 (`X-Lux-SHA256`), so a download cut short is
   detected.
4. Retention deletes a finished Run's blobs from S3 after the tenant's
   `retention_days`.

Keys are `tenants/<tenant>/runs/<run>/<blob>`. Encrypt the bucket at rest
(SSE-KMS on AWS).

## Hosts

Host requirements:

- Linux with cgroup v2. The runner makes cgroups of its own under
  `/sys/fs/cgroup/lux.slice` for image builds (their process limit, and
  ending everything a cancelled build started), and enables the `pids`
  controller for them.
- Podman ≥ 5 with netavark, run rootful.
- A `containers` range in `/etc/subuid` and `/etc/subgid` (for
  `--userns=auto`), for example `containers:2147483647:2147483648`.
- Podman storage on the native `overlay` driver, not fuse-overlayfs.
  Image builds run in their own user namespace, and under fuse-overlayfs
  their `RUN` steps can't write to the image's root directory.
- Tested with Podman 5.8. The runner turns idmapped overlay mounts off for
  builds (`_CONTAINERS_OVERLAY_DISABLE_IDMAP`, containers/storage's only
  switch for it). Without that, a build's files are owned by host ids, and
  its image id depends on the uid range it happened to get. The e2e test
  `test_rebuilds_match_whatever_uid_range_they_get` catches a Podman
  upgrade that drops the switch: run it before upgrading hosts.
- nftables (the runner owns the `inet lux` table; see
  [egress](runspec.md#network-egress)).
- For nested containers, `lux-runner --nested`: the host needs `/dev/fuse`
  and `/dev/net/tun`. The runner then labels the host `nested=true`; that
  label cannot be set with `--label` or a host token.

```bash
LUX_URL=https://luxd.example LUX_HOST_TOKEN=luxh_… lux-runner --name host-a
```

| Flag | Default | |
| --- | --- | --- |
| `--name` | hostname | Unique within the tenant. |
| `--data-dir` | `/var/lib/lux` | Run state and local snapshot copies. |
| `--shim` | `/usr/local/lib/lux/lux-shim` | The shim binary to mount into containers. |
| `--label k=v` | | Host labels, matched by `placement.requires` and `prefers`. Also `LUX_LABELS=k=v,…`. |
| `--max-runs`, `--cpus`, `--memory` | 16, all, all | Capacity offered to the scheduler. |
| `--disk` | not reserved | Bytes of disk the scheduler reserves Runs' `resources.disk` from. Without it, disk is not reserved (Runs still stop at their own limit). Set it to the space under `/var/lib/containers`, more to overcommit. |
| `--usage-every` | `15s` | How often each Run's disk use is sampled, and its disk limit checked. |
| `--host-ttl` | `24h` | How long uploaded local copies are kept, and images lux pulled or built after their last use ([images on hosts](runspec.md#images-on-hosts)). |
| `--image-disk-high` | `80` | Percent use of the disk holding Podman's storage (its graph root, `/var/lib/containers`) over which lux's unused images are removed, least recently used first, until under it. `0` turns it off. Images the host had before lux are never removed. |
| `--provider-id` | | The cloud instance id, for provisioned hosts. Also `LUX_PROVIDER_ID`. |
| `--ec2-imds` | off | EC2 instance metadata URL (`http://169.254.169.254`) to watch for spot interruptions. Also `LUX_EC2_IMDS`. |
| `--poll` | off | Use HTTP polling instead of a WebSocket. There is no live output in this mode: output arrives after exit. |

Private registries ([`image.registryAuth`](runspec.md#private-registries))
should be HTTPS. For a plain-HTTP registry (a test or air-gapped one),
list it as insecure on every host:

```bash
cat > /etc/containers/registries.conf.d/50-internal.conf <<'EOF'
[[registry]]
location = "10.0.0.5:5000"
insecure = true
EOF
```

Host tokens come from `luxd admin create-host-token --tenant T [--pool P]
[--label k=v]`. Restarting `lux-runner` does not touch running containers
(Podman is daemonless). The new runner re-adopts them.

### Binary distribution and self-update

luxd serves the runner binaries a host needs, so no custom AMI or
config-management run has to carry them:

- `GET /runner/bin/manifest` (host-token auth): `{"linux-arm64":
  {"lux-runner": "<sha256>", "lux-shim": "<sha256>"}, "linux-amd64": {...}}`
  for whatever `LUX_RUNNER_BIN_DIR` holds.
- `GET /runner/bin/linux-{arch}/{lux-runner|lux-shim}` streams the binary,
  with `Content-Length` and its sha256 in `X-Lux-Sha256`.

luxd hashes what it finds under `LUX_RUNNER_BIN_DIR` once at startup; a
missing arch or file is simply not offered. A host downloads its arch's
binaries before starting `lux-runner` (its systemd unit's
`ExecStartPre`), so an upgrade is: replace luxd's `runner_bin_dir`,
restart luxd, then restart or replace each host — never patch a running
binary in place.

Every `Hello` and `Heartbeat` a runner sends carries the sha256 of its own
binary (`os.Executable()`) and of its `--shim`; older runners simply omit
them. When luxd holds binaries for that host's arch and either sha
differs, it drains the host once (reason `outdated binaries`; a host
already draining for another reason is left alone, and this never
re-drains a host on a later heartbeat). A drained **provisioned** host is
terminated once idle, same as any other drain, and the pool launches a
fresh one with the running luxd's binaries. A drained **static** host is
told to exit, once it is idle and has nothing left to upload: its systemd
unit's `Restart=always` brings it back, and its `ExecStartPre`
re-downloads the binaries first.

## EC2 pools

A pool with `provider: ec2` is sized by luxd:

```bash
lux pools set burst --provider ec2 --min 0 --max 10 --warm 1 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner","instanceType":"m7i.2xlarge","subnets":["subnet-a","subnet-b"]}'
lux pools rm burst      # drains and terminates its hosts
```

- **Scale up:** when Runs for the pool wait in `provisioning`, luxd launches
  enough hosts for them, plus `--warm` idle ones kept ready. It keeps at
  least `--min` hosts and never more than `--max`. Launches alternate
  across the template's subnets.
- **Scale down:** a host idle longer than `LUX_SCALE_DOWN_AFTER` (default
  10m), above the minimum and warm count, is drained. It is terminated once
  it has no live placements and nothing left to upload. The same applies
  to `lux pools rm`, and a Run on a drained host is stopped (snapshotted),
  never cut short.
- **Failures:** a launch that fails is retried on the next pass. A host
  that never registers within `LUX_LAUNCH_TIMEOUT` (default 10m) is
  terminated. An instance EC2 no longer has is written off and replaced.
- **Orphans:** once a minute luxd lists the pool's instances by tag. One
  no host row claims (a launch whose reply was lost) is terminated; a host
  EC2 no longer lists (and whose runner is silent) is terminated and
  written off. A host lost for over 5 minutes is
  terminated.
- One luxd instance does all this at a time (a lease in Postgres).

### Spot instances

Add `"spot": true` to the template to launch one-time spot instances:

```bash
lux pools set burst --provider ec2 --max 10 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner","spot":true}'
```

EC2 gives two minutes' notice before it takes a spot instance back. Each
instance's runner watches for it (user data sets `LUX_EC2_IMDS`, which the
runner reads as `--ec2-imds`; IMDSv2 must be reachable). On the notice:

1. The runner tells luxd, which drains the host: nothing new is placed on
   it, and each of its Runs is asked to stop with reason `preempt`.
2. Stops from then on get a grace of at most half the time left, so the
   Run has time to snapshot its state volumes and upload them.
3. Each preempted Run is resumed automatically from that snapshot, on
   another host. If the pool has none free it launches one. An agent picks
   its session back up, as after any resume.

A Run whose stop or upload cannot finish in time (a very large state
volume) is lost when the instance goes. It is resumable from its previous
snapshot. Keep state volumes small on spot pools, or use on-demand for
Runs that cannot afford it (`placement.pool`).

What an instance needs:

- An AMI with the host requirements above, `lux-runner` and `lux-shim`,
  and a boot script that reads the instance's **user data** (`KEY=value`
  lines: `LUX_URL`, `LUX_HOST_TOKEN`, `LUX_HOST_NAME`, `LUX_EC2_IMDS`) and runs
  `lux-runner --provider-id <instance id>` with them in its environment.
  The token is single-use per host and revoked when the host is
  terminated.
- A launch template (id `lt-…` or name) with that AMI, a security group
  that reaches luxd, and an instance profile if the runner needs one (it
  doesn't hold S3 credentials).
- luxd needs EC2 permissions for `RunInstances` (with the launch template
  and `CreateTags`), `TerminateInstances` and `DescribeInstances`, from its
  standard AWS configuration (environment or instance role).
  `LUX_EC2_ENDPOINT` overrides the endpoint.

Instances are tagged `Name=<host>`, `lux:pool=<pool>` (`<tenant>/<pool>`
for a tenant's pool), `lux:managed=true`, `lux:deployment=<id>` (which lux
database launched it: deployments sharing an account never touch each
other's instances) and `lux:host=<host id>`, plus the
template's `tags`. luxd also needs `DescribeInstances` filtered by tag.

Reusable Terraform for running all of this on AWS — control host, S3,
runner launch templates, Cloudflare Tunnel — is under
[deploy/terraform/](../deploy/terraform/README.md).
