# The RunSpec

A RunSpec describes what a Run executes. You submit it as YAML or JSON
(`lux run -f spec.yaml`). luxd validates it and fills in defaults,
reporting every problem at once. It stores the result without secret
values.

```yaml
name: fix-flaky-test            # optional, for humans
labels: { team: payments }      # free-form; filter with `lux ls -l team=payments`

image:
  ref: ghcr.io/acme/agent@sha256:…        # exactly one of ref or build

workload:
  adapter: claude-code          # generic | acp | claude-code | codex | opencode
  command: [claude, --model, sonnet]      # default: the adapter's
  prompt: "Fix the flaky test in tests/api"
  workdir: /workspace/repos/api
  user: agent                   # default: the image's USER
  grace: 30s                    # graceful stop before SIGKILL
  resume: { command: [...] }    # generic only: what to run on resume

init:
  script: npm ci                # runs before the workload, on every start

env: { NODE_ENV: test }

secrets:                        # values supplied by the caller; never stored
  - { name: ANTHROPIC_API_KEY, value: sk-…, as: env }
  - { name: npmrc, value: "…", as: file, path: /home/agent/.npmrc }
  - { name: GITHUB_TOKEN, value: ghp_… }   # used as a git credential below

git:
  repositories:
    - name: api
      url: https://github.com/acme/api.git
      ref: main                 # branch, tag or sha; default: the remote's HEAD
      credential: GITHUB_TOKEN  # a secret name; the runner uses it, the container never sees it
      path: /workspace/repos/api  # default: /workspace/repos/<name>
  push:
    branch: lux/fix-flaky-test  # where `lux push` pushes

volumes:
  - { name: workspace, path: /workspace, kind: state }     # snapshotted on every exit
  - { name: home, path: /home/agent, kind: state }
  - { name: cache, path: /cache, kind: ephemeral }         # empty on every start

resources: { cpus: 4, memory: 8Gi, pids: 2048 }
timeout: 4h                     # wall-clock across all placements

placement:
  pool: default
  requires: { arch: amd64 }     # host labels that must match
  prefers: { region: eu-west-1 }

network:
  egress:                       # default deny: only these are reachable
    - host: api.anthropic.com
    - cidr: 140.82.112.0/20
  # unrestricted: true          # no egress filtering (the hard blocks still apply)
  ports:
    - { port: 3000, name: web }

sandbox:
  nestedContainers: false
  readOnlyRoot: false

artifacts:
  paths: ["/workspace/out/**"]
```

## Rules

- `image`: exactly one of `ref` or `build`.
- `workload.adapter` decides how the workload is started, steered, stopped
  and resumed (see [adapters](adapters.md)). Agent adapters need their
  session paths on a state volume (for example `/home/agent` for Claude
  Code's `~/.claude`). A spec that leaves them off is rejected, because it
  would lose its conversation on the first resume.
- `env` names must be valid variable names and must not start with `LUX_`.
- Secrets are `env` (the default) or `file` (placed on a tmpfs and linked
  at `path`). A file secret is never in a snapshot. Every resume must
  supply every secret again.
- Volume paths must be absolute. `/.lux` is reserved.
- Resources default to 2 CPUs, 4 GiB of memory, 1024 PIDs. Sizes accept
  `512Mi`, `8Gi`, `1G`, or bytes.

## Images

`image.ref` names an image. Pin it by digest (`name@sha256:…`) for a Run
that is the same everywhere. It is pulled once per host and cached.

`image.build` builds a Containerfile on the host that runs the Run. There
is no registry:

```yaml
image:
  build:
    containerfile: |
      FROM docker.io/library/node:24
      RUN npm install -g pnpm
    args: { NODE_ENV: test }      # --build-arg
```

- **The first build pins every `FROM` to a digest.** The pinned
  Containerfile and the image id are recorded on the Run (`lux get` shows
  them). Every later placement, on any host, builds from the pinned form,
  so a tag that moves in between does not change the base.
- A host builds each distinct pinned Containerfile, args, tenant and
  network rules once, and reuses the image after that. A build's result
  depends on what it could reach, so no image, and no layer cache, is
  shared between tenants or between different egress rules. Built images a
  host has not used for its host TTL are removed.
- `COPY --from` and `RUN --mount=from=` may name earlier stages only. An
  image named there would not be pinned, so it is refused. Add
  `FROM image AS name` and use the name instead.
- A build has the Run's limits: CPUs, memory (no swap), and processes.
- A stop or cancel during a build ends the build.
- If a rebuild on another host produces a different image, the Run
  continues and records an `image.rebuild-differs` event. That happens when
  a `RUN` step is not reproducible, for example one that downloads the
  latest of something. State volumes do not depend on the image.
- **Builds are contained like the workload:** their own user namespace, the
  same dropped capabilities, the Run's resource limits, and the Run's
  network, so a `RUN` step has the Run's egress (and needs its registries
  and package mirrors allowed).
- A `FROM` naming its image through a build argument can't be pinned and
  is refused. `scratch` and earlier stages are left alone.
- Not yet: a build context. `COPY` and `ADD` have nothing to copy from, so
  a spec with `image.build.context` is refused.

## Nested containers

`sandbox.nestedContainers: true` lets the workload run containers itself,
with rootless Podman inside its container. The Run is placed only on hosts
started with `lux-runner --nested`. Its image must have Podman (and
`fuse-overlayfs`, and `newuidmap`/`newgidmap`), and the workload user
needs `/etc/subuid` and `/etc/subgid` entries. `tests/images/nested` is a
minimal example.

A nested Run is never `--privileged`. Beyond what every Run gets, it gets:

- `CAP_SYS_CHROOT` in its bounding set;
- no `no-new-privileges`, so that `newuidmap`/`newgidmap` can take
  `CAP_SETUID`/`CAP_SETGID` from their file capabilities. The workload
  itself starts with no capabilities. But without `no-new-privileges`, any
  setuid-root program in the image (`sudo`, `su`) can make it root in its
  container, exactly as a Run whose workload runs as root. That is within
  the Run: container root is an unprivileged uid range on the host, with
  the same bounding set as every Run. Leave setuid programs out of nested
  images if the workload should stay unprivileged in its own container;
- `/dev/fuse` and `/dev/net/tun`;
- `unmask=ALL` and `label=disable`;
- the host's seccomp profile, plus `sethostname`, `setdomainname` and
  `setns` (normally allowed only with `CAP_SYS_ADMIN`, which it does not
  get).

A host started without `--nested` refuses nested Runs.

It is still in its own user namespace (an unprivileged uid range on the
host). The containers it starts use the Run's network, so they have its
egress rules and hard blocks, and nothing more.

## Artifacts

Artifacts are files a Run produces, kept after it ends and downloadable
with `lux artifacts <run> --download DIR`. They are collected **on every
exit** (a stop, a failure, a cancel, not only success), per placement:

- files matching `artifacts.paths`: absolute globs on the Run's volumes,
  where `*` matches within a directory and `**` any depth
  (`/workspace/out/**`, `/workspace/**/*.xml`);
- anything the workload writes into `$LUX_ARTIFACTS` while it runs,
  listed as `/.lux/artifacts/<name>`. That directory is emptied once
  collected, so each placement publishes its own.

Each artifact is stored like a snapshot blob (uploaded through luxd to S3)
with its size, sha256 and content type, and listed by placement epoch.
Downloads stream through luxd as the file the Run wrote.

Limits: 1000 artifacts per placement, 1 GiB per file. Symlinks are never
collected: a workload's link could point anywhere on the host.

## Git

lux clones repositories for you, on the host, before the container
starts:

1. Each repository is fetched into a **mirror** on the host. The mirror is
   shared by every Run there and updated on use, so a second Run on the
   same host fetches only what is new. The scheduler prefers hosts that
   already hold the mirrors a spec needs.
2. It is cloned from the mirror into the Run's `path`, which must be on a
   **state volume**, at `ref`: a branch checked out as a local branch, or a
   tag or sha detached. The clone is self-contained, so it moves with the
   Run to hosts that have no mirror.
3. The checkout's `origin` is the plain `url`. The credential is used by
   the runner for the fetch, and later the push. It is **never in the
   container**: not in the environment, not in `.git/config`, and not on
   the host's disk. A `url` that carries credentials (`https://user:tok@…`)
   is refused; use `credential:`.
4. On a **resume**, the checkout is already on the restored volume and is
   left exactly as the workload left it.

`lux push <run> [--wait]` pushes each repository's current commit to
`git.push.branch`, again with the runner's credential:

- The push is **leased**: it replaces the branch only if the branch still
  points where this Run last pushed it (or, the first time, only if the
  branch does not exist). If someone else pushed in between, the push is
  `rejected` and their commit stays. luxd keeps the lease, not the
  checkout.
- **The runner never runs git in the checkout.** The workload controls its
  `.git`, including hooks and config such as URL rewrites. So the push works
  from a bundle: the workload's user writes a bundle of `HEAD` inside the
  container, and the runner pushes that bundle from a repository it owns.
  The checkout's hooks and config never run as the runner and never see the
  token.
- Pushing with nothing new reports `up-to-date`.
- Each repository's outcome is a `git.push` event. `--wait` prints the
  outcomes and exits non-zero unless every repository was pushed or already
  up to date.

The workload commits as it likes. The checkout is a normal git repository
owned by the workload user.

## Network egress

**Default deny.** A Run can reach only the CIDRs and hostnames in
`network.egress`. A spec with no rules reaches nothing.

- **CIDRs** are allowed as written.
- **Hostnames** are resolved by the runner (not the container) when the Run
  starts, and every minute after that. New addresses are added and none are
  removed while the Run lives, so a connection is not cut when a CDN
  rotates. Wildcards can't be resolved, so list the concrete hostnames.
- **DNS:** the Run's resolver is a stub on its network's gateway. It
  answers only allowed names, with exactly the addresses the firewall
  allows. It refuses every other name and records each distinct lookup
  (name, allowed or not) as a `dns` event, so "what did this Run try to
  reach" has an answer. Queries sent to
  any other DNS server are redirected to the stub, so DNS can't be used to
  get data out.
- **IPv4 only.** Allow rules are IPv4; a restricted Run's IPv6 traffic is
  dropped, and the stub answers no AAAA queries.
- **Always blocked, whatever the spec says:** link-local and the cloud
  metadata services (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`),
  loopback, the control plane, and other Runs on the same host. An allowed hostname that resolves into a blocked range does
  not open it.
- `unrestricted: true` switches the filtering off, and the Run resolves
  names through the host's resolvers. The hard blocks above still apply.
- **A known limitation, by choice:** rules match IP addresses, so allowing
  a name hosted on a CDN allows everything else on those shared addresses.
  Filtering on hostnames (SNI) through a proxy is the upgrade path.

How it works: each Run gets its own bridge network, with Podman's own DNS
turned off. The runner owns an nftables table, `inet lux`, which netavark
never touches:

- Traffic from each Run's bridge is sent to that Run's chain, which allows
  replies and the Run's allow set, drops the hard-blocked ranges, and drops
  everything else.
- Traffic from a lux bridge with no rules loaded, for example while the
  runner restarts, is dropped, so a Run fails closed, never open. The
  runner replaces the table in one transaction, so there is no moment
  without it. A restarted runner re-applies the rules for the Runs it
  re-adopts before anything else.
- The rules and the DNS stub are in place before the container starts,
  so its first packet and its first lookup are covered. They are removed
  when the container exits.
- The control plane is blocked by every address its URL's host resolves
  to when the runner starts.
