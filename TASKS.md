# lux — build progress

Tracks the v1 build order.
Each step ends with an end-to-end test through `tests/run_tests.py`.

| # | Step | Status |
| --- | --- | --- |
| 0 | Test harness (`--infra-only`) | ✅ done |
| 1 | Skeleton: module, migrations + RLS, `luxd migrate/admin/serve`, API keys, runs API, runner registration, WebSocket + poll, `generic` in hardened Podman, CLI | ✅ done |
| 2 | Output: shim as PID 1, output files, `logs -f` via SSE relay, cursors | ✅ done |
| 3 | Snapshots + blob store, stop/resume same host and cross host, epochs/fencing — the migration test | ✅ done |
| 4 | Steering + adapters: `POST /input`, `lux-fake` over ACP, `acp` adapter, `claude-code` | ✅ done |
| 5 | S3 (straight through luxd), presigned downloads, host-local GC, retention | 🚧 in progress |
| 6 | Git workspace: mirrors, clone at ref, push with runner-held credentials | ⏳ |
| 7 | Egress: per-Run networks, nftables, DNS stub, re-resolution | ⏳ |
| 8 | Secrets: tmpfs, redaction, re-supply on resume | ⏳ |
| 9 | Images from Containerfiles: `podman build`, `FROM` pinning, rebuild warnings | ⏳ |
| 10 | Nested containers (opt-in) | ⏳ |
| 11 | Attach, exec, port forwarding through the relay | ⏳ |
| 12 | Artifacts | ⏳ |
| 13 | EC2 pools (fake EC2 in tests, `--real-ec2` for nightly), autoscaling, draining, warm pools | ⏳ |
| 14 | Codex and OpenCode adapters | ⏳ |

Also: docs in `docs/` kept current with each step; external-tool findings
recorded in `docs/agent-protocols.md` and `docs/podman.md`.
