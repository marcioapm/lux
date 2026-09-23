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
3. Downloads are presigned S3 URLs, valid for 15 minutes. A runner asks
   `GET /runner/blobs/{id}`, and luxd redirects it only for blobs of a Run
   placed on that host. Clients download artifacts the same way, or through
   luxd with `?proxy=true`.
4. Retention deletes a finished Run's blobs from S3 after the tenant's
   `retention_days`.

Keys are `tenants/<tenant>/runs/<run>/<blob>`. Encrypt the bucket at rest
(SSE-KMS on AWS).

## Hosts

Host requirements:

- Linux with cgroup v2.
- Podman ≥ 5 with netavark, run rootful.
- A `containers` range in `/etc/subuid` and `/etc/subgid` (for
  `--userns=auto`), for example `containers:2147483647:2147483648`.
- nftables (the runner owns the `inet lux` table; see
  [egress](runspec.md#network-egress)).
- `/dev/fuse` for nested containers.

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
