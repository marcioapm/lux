# Operations

## luxd

`luxd` is stateless. Postgres holds state and S3 holds bytes, so you can
run several instances behind a load balancer. Each runner holds one
WebSocket to one instance.

```bash
luxd migrate      # once per upgrade, as the database owner
luxd serve        # as many as you like
```

| Variable | Default | |
| --- | --- | --- |
| `LUX_DATABASE_URL` | — | For `serve`, a DSN for the `lux_app` role (created by `migrate`, which needs the owner's DSN and takes `LUX_APP_PASSWORD`). |
| `LUX_LISTEN` | `127.0.0.1:7070` | Address to serve on. |
| `LUX_PUBLIC_URL` | — | The URL runners and clients use. |
| `LUX_S3_BUCKET` | — | Where snapshots, output and artifacts go. |
| `LUX_S3_ENDPOINT` | AWS | For MinIO and other S3-compatible stores (path-style). |
| `LUX_S3_PUBLIC_ENDPOINT` | = endpoint | The endpoint presigned URLs are signed for, if runners and clients reach S3 by another name. |
| `LUX_S3_REGION` | `us-east-1` | |
| `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY` | AWS chain | Only luxd holds S3 credentials. |
| `LUX_LEASE` | `30s` | A host that misses heartbeats this long is lost, along with its live placements. |
| `LUX_TICK` | `1s` | Scheduler and reaper interval. |
| `LUX_DEFAULT_CPUS`, `LUX_DEFAULT_MEMORY`, `LUX_DEFAULT_PIDS` | `2`, `8Gi`, `1024` | Resources a Run gets when its spec sets none. |
| `LUX_SCALE_DOWN_AFTER` | `10m` | How long a provisioned host stays idle before it is drained and terminated. |
| `LUX_LAUNCH_TIMEOUT` | `10m` | How long a launched host may take to register before it is terminated. |
| `LUX_EC2_ENDPOINT` | AWS | Overrides the EC2 endpoint (tests). |
| `LUX_DEBUG` | — | Debug logging. |

### Tenants, keys and quotas

```bash
luxd admin create-tenant --name acme [--max-runs N] [--max-hosts N] [--retention-days 30]
luxd admin create-key --tenant T --scopes read,run
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
| `--host-ttl` | `24h` | How long uploaded local copies are kept. |
| `--poll` | off | Use HTTP polling instead of a WebSocket. There is no live output in this mode: output arrives after exit. |

Host tokens come from `luxd admin create-host-token --tenant T [--pool P]
[--label k=v]`. Restarting `lux-runner` does not touch running containers
(Podman is daemonless). The new runner re-adopts them.

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
- One luxd instance does all this at a time (a Postgres advisory lock).

What an instance needs:

- An AMI with the host requirements above, `lux-runner` and `lux-shim`,
  and a boot script that reads the instance's **user data** (`KEY=value`
  lines: `LUX_URL`, `LUX_HOST_TOKEN`, `LUX_HOST_NAME`) and runs
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

Instances are tagged `Name=<host>`, `lux:pool=<pool>`, `lux:managed=true`,
plus the template's `tags`.
