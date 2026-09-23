# Development

## Layout

```
cmd/            one package per binary: luxd, lux-runner, lux-shim, lux, lux-fake
internal/
  server/       luxd: API, runner hub, scheduler, reapers, output relay
  store/        Postgres access, RLS scopes, migrations (store/migrations/*.sql)
  blob/         S3
  runner/       lux-runner: placements, snapshots, uploads, re-adoption
  podman/       Podman through its CLI
  shim/         lux-shim: PID 1, output file, redaction
  adapter/      generic, acp, claude-code, codex
  proto/        luxd ↔ runner and runner ↔ shim messages
  spec/         the RunSpec: parsing, validation, defaults
  cli/, client/ the CLI and its API client
tests/          the end-to-end harness (Python, uv, pytest)
docs/
```

## Building

```bash
make build        # static binaries in bin/
make lint         # go vet, gofmt
make unit         # go test ./... (store tests need Postgres; see below)
```

The toolchain and every dependency are kept at their latest release.

## The end-to-end harness

lux's real bugs live at the seams between luxd, the runner, Podman, the
container and the shim, where unit tests cannot see. So every feature is
tested end to end through `tests/run_tests.py`. Tests drive **the `lux`
CLI** wherever possible, which tests the CLI and the behaviour at once. They
go around it only for admin bootstrap (`luxd admin`), for things no client
can see (the database, a host's disk, nftables), and for fault injection.

```bash
cd tests
uv run python run_tests.py                   # build, bring everything up, run all suites
uv run python run_tests.py --infra-only      # only check the environment itself
uv run python run_tests.py suites/test_x.py  # one suite; -x, -k work as in pytest
uv run python run_tests.py --keep            # keep containers and database to debug
uv run python run_tests.py --hosts 3         # more simulated hosts
uv run python run_tests.py --real-ec2        # EC2 suites against real AWS (nightly)
```

Each invocation:

1. Builds every `cmd/<name>` into `bin/` (static) and the fake agent's
   image (`tests/images/fake`).
2. Starts (or reuses) a shared **Postgres** (`lux-e2e-postgres-18`, port
   55432) and **MinIO** (`lux-e2e-minio`, port 59000), and creates a
   database and a bucket for this run.
3. Creates a Docker network and N **simulated hosts**. Each host is a
   privileged `quay.io/podman/stable` container with its own Podman
   storage, `bin/` mounted at `/opt/lux`, a `containers` range in
   `/etc/subuid`, and the test images preloaded.
4. Runs `luxd migrate` and then `luxd serve` on the network's gateway
   address, which the hosts, the tests and presigned MinIO URLs can all
   reach.
5. Runs pytest. Fixtures start runners inside hosts
   (`runners.start(hosts[0])`), so a test can kill a host, restart a runner
   or stop one mid-Run.
6. Tears everything down. Logs stay in the directory it prints:
   `luxd.log`, `host-a/runner.log`, `host-a/podman-ps.txt`, …

The store unit tests create throwaway databases in the harness's Postgres:

```bash
LUX_TEST_PG='postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable' go test ./internal/store
```

### Agent harnesses: one set of tests for every agent

Every coding agent lux drives is described once, in `tests/harnesses.py`:
its adapter, how to build a spec for it, which credentials it needs, and
what its protocol can do (`Caps`). `tests/suites/test_agents.py` takes a
`harness` fixture, so each test runs once per agent, in two variants:

| Variant | What runs | When |
| --- | --- | --- |
| `<agent>-fake` | `lux-fake` speaking that agent's protocol | always |
| `<agent>-real` | the real CLI, with real model calls | with `-m agents` and its credentials |

Tests assert from capabilities, never from an agent's name. For example,
input sent mid-turn joins the running turn only where
`caps.steer_joins_turn` (Codex). **Adding an agent is one `Harness` entry**
(plus a `lux-fake` protocol mode if it speaks a new protocol). Every
existing test then covers it. Use `@harnesses(pred)` to limit a test to
the agents it concerns.

`lux-fake` speaks all three protocols: ACP by default, Claude Code's
stream-json (`-p --input-format stream-json …`), and Codex's app-server
(`app-server`). It follows a script from the prompt (`write f text`,
`sleep 5`, `history`, …; see `cmd/lux-fake/main.go`), keeps a transcript on
a state volume, and resumes from it. Its ACP replies stream in chunks
without line breaks, as real agents' do.

### Real agents (opt-in)

The `-real` variants make model calls, so each runs only when its
credentials are set and the suite is selected with `-m agents` (which also
makes the harness build the agents' images):

| Agent | Variables |
| --- | --- |
| Claude Code | `LUX_TEST_ANTHROPIC_API_KEY`, optional `LUX_TEST_ANTHROPIC_BASE_URL` |
| Codex | `LUX_TEST_OPENAI_API_KEY`, optional `LUX_TEST_OPENAI_BASE_URL`, `LUX_TEST_CODEX_MODEL` |
| OpenCode | `LUX_TEST_OPENCODE_AUTH` (an `auth.json`), `LUX_TEST_OPENCODE_CONFIG` (an `opencode.json`), `LUX_TEST_OPENCODE_MODEL` (`provider/model`) |

```bash
LUX_TEST_ANTHROPIC_API_KEY=sk-… uv run python run_tests.py suites/test_agents.py -m agents
```

The harness copies each CLI installed on the developer machine (the native
binary, never a Node launcher) into a test image built from
`tests/images/agent`. Credentials go in as secrets, never as mounted
config.

### Guards are mutation-checked

When you add a guard (fencing, RLS, redaction), break it on purpose and
watch its test fail before you trust the test.
