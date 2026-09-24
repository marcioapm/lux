# CLI

`lux` talks to luxd. It reads its configuration from flags, then the
environment, then `~/.config/lux/config.toml`:

```toml
url = "https://luxd.example.com"
api_key = "lux_…"
```

| Flag | Env | |
| --- | --- | --- |
| `--url` | `LUX_URL` | luxd's address |
| `--api-key` | `LUX_API_KEY` | an API key (scopes: `read`, `run`, `admin`; or an operator key) |
| `--tenant` | `LUX_TENANT` | with an operator key: one tenant only (id or name) |
| `-o json` | | machine-readable output, for every command that prints data |

**Exit codes:** `0` success; the Run's own exit code for `run --follow`,
`run --wait`, `resume --follow` and `wait`; `3` not found; `4` conflict or
invalid spec (for example, steering a stopped Run); `5` a tenant quota
was reached; `1` anything else.

## Runs

```bash
lux run -f spec.yaml [--follow | --wait] [--name N] [-l k=v] [--idempotency-key K] [--secrets-from .env]
lux run --image alpine -- echo hello           # a quick generic Run
lux ls [--state running,stopped] [-l team=x] [--resumable] [--host H] [--limit N]
lux get <run>                                  # state, placements, usage
lux logs <run> [-f] [--since <cursor>] [--events] [--stderr=false]
lux events <run>                               # lifecycle events
lux wait <run> [--state s1,s2] [--timeout 5m]  # default: until it ends; exits with its code
```

Specs are YAML or JSON (`-f -` reads stdin). A secret's value can come from
the environment (`value: ${GITHUB_TOKEN}`), from a `.env` file
(`--secrets-from`), or, when the spec omits the value, from an environment
variable of the same name.

`lux logs -o json` prints one JSON record per line:
`{"cursor","epoch","seq","t","ch","data"|"event"}`. Pass a record's
`cursor` to `--since` to continue from it, across placements and hosts.

## Steering

```bash
lux steer <run> "also add a test" [--interrupt] [--request-id ID]
lux interrupt <run>
```

Agents get the text as a message. Generic workloads get it on stdin. See
[adapters](adapters.md) for when a message is delivered.

## Stop, resume, cancel

```bash
lux stop <run> [--wait]         # graceful; snapshot; resumable
lux resume <run> [--wait | --follow] [--input "..."] [--secret NAME=VALUE] [--secrets-from .env] [--from-snapshot ID] [--disk SIZE] [--to HOST]
lux cancel <run> [--wait]       # final (a snapshot is still taken)
lux snapshots <run>             # where each snapshot lives
```

A resume needs the Run's secrets again. lux looks for each one in
`--secret`, then `--secrets-from`, then an environment variable of the same
name.

## Git

```bash
lux push <run> [--wait]         # push each repository to git.push.branch (leased)
```

## Files

```bash
lux artifacts <run> [--download DIR]
```

## Hosts and pools

```bash
lux hosts ls [--all] [--pool P] [--state S]
lux hosts get <host>            # lifecycle, capacity, allocation, live Runs
lux hosts drain <host>          # admin: move its Runs elsewhere, place nothing new
lux pools ls
lux pools set <name> --provider static|ec2 [--min N] [--max N] [--warm N] [--template JSON]
```

## Status and history

```bash
lux status                      # Runs by state, queue, time to start, hosts, capacity now
lux history [--since 24h]       # the same over time
lux history <run>               # a Run's resource use, across placements
lux history --host <host>
lux events --all                # every Run's events as they happen
```

## Operators

With an operator key, every command covers every tenant (`--tenant`
narrows it), and there is more: `lux tenants ls`, `lux migrate`,
`lux resume --to`. See [Operators and the console](operators.md).

## Interactive

All three go through luxd, which relays to the Run's host. The client
never talks to the host. They need the Run `running` on a host with a live
connection.

```bash
lux exec <run> [-t | -T] -- command...
lux attach <run>
lux port-forward <run> <port-name> <local-port> [--address 127.0.0.1]
```

- **exec** runs a command in the container as the workload user, with the
  workload's environment and working directory. Its stdin, stdout, stderr
  and exit code are yours. A terminal is allocated when your stdin is one
  (`-t` forces it, `-T` turns it off). If the client goes away, the
  command is killed. Exec output is not part of the Run's output.
- **attach** joins the terminal of a `generic` workload started with
  `workload.tty: true`: its output from now on, and your typing. Ctrl-]
  detaches; the workload keeps running. What the workload prints on its
  terminal is also the Run's output, as always.
- **port-forward** listens locally and tunnels each connection to a port
  the Run declares in `network.ports`, by name. Undeclared ports cannot be
  reached.
