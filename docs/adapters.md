# Adapters

lux is agent-agnostic: a Run is a process in a container. An **adapter**
connects lux's four verbs to whatever the process speaks on stdio:

| Verb | What it means |
| --- | --- |
| **deliver input** | Hand a message to the workload (`lux steer`). |
| **interrupt** | Stop the workload's current turn, not the workload. |
| **stop** | Wind the workload down gracefully before the container exits. |
| **report** | Tell lux what was learned: session id, idle or busy, delivery acknowledgements, structured events. |

The adapter runs inside the container, in `lux-shim`. Pick one with
`workload.adapter` in the [RunSpec](runspec.md).

| Adapter | Process | Input | Mid-turn input | Interrupt | Stop | Resume |
| --- | --- | --- | --- | --- | --- | --- |
| `generic` | `command` as given | text + newline (or raw bytes) on stdin | — | SIGINT | SIGTERM | re-runs `resume.command`, or `command` |
| `acp` | any [ACP](https://agentclientprotocol.com) agent | `session/prompt` | queued until the turn ends | `session/cancel` | cancel, close stdin | `session/load` if the agent supports it |
| `claude-code` | `claude -p --input-format stream-json --output-format stream-json --verbose` | a `user` JSON line | native (Claude Code queues it) | `control_request` interrupt | **SIGINT** (ends the turn cleanly) | `--resume <session id>` |
| `codex` | `codex app-server` | `turn/start` | native (`turn/steer`) | `turn/interrupt` | interrupt, close stdin | `thread/resume` |
| `opencode` | `opencode acp` | as `acp` | as `acp` | as `acp` | as `acp` | as `acp` |

The exact messages, verified against the real CLIs, are in
[agent-protocols.md](agent-protocols.md). Each adapter is tested end to end
against `lux-fake` speaking its protocol, and against the real CLI (Claude
Code, Codex and OpenCode) in an opt-in suite: steered, stopped, and resumed
on another host with its conversation intact.

## Credentials

Agents authenticate however they normally do, through secrets:

- **Claude Code:** `ANTHROPIC_API_KEY` as an env secret.
- **Codex:** Codex reads its key from `~/.codex/auth.json`, not the
  environment. Pass `{"auth_mode":"apikey","OPENAI_API_KEY":"…"}` as a file
  secret at that path.
- **OpenCode:** its `auth.json` (keys) and `opencode.json` (providers and
  model) as file secrets under `~/.local/share/opencode/` and
  `~/.config/opencode/`.

File secrets live on a tmpfs, so they are never snapshotted, and they are
supplied again on every resume.

## What every adapter gives you

- **Idle or busy.** The Run's `activity` field says whether an agent is
  working or waiting for input. `lux ls` shows `running (waiting for
  input)` instead of leaving you to guess whether it is hung.
- **Acknowledged input.** Every `lux steer` gets a request id. A delivered
  input shows up as an `input.delivered` event, and a failed one as
  `input.failed`. The shim delivers each id once, so retrying a steer
  (`--request-id`) never sends it twice.
- **Structured events.** The agent's own protocol messages are kept as
  events in the output (`lux logs --events`). The reply text also goes to
  stdout, so `lux logs` reads like a conversation.
- **The session id.** It is stored on the Run and passed back on resume, so
  the conversation continues from its transcript on the restored state
  volume.

## State paths

An agent keeps its session in files, and those files must be on a
**state volume**, or the first resume starts a new conversation. lux
rejects a spec that gets this wrong, at submission:

| Adapter | Session lives in |
| --- | --- |
| `claude-code` | `$HOME/.claude` |
| `codex` | `$HOME/.codex` |
| `opencode` | `$HOME/.local/share/opencode` |

Put the agent's home on a state volume, for example
`{ name: home, path: /home/agent, kind: state }`.

## Unattended permissions

Nobody is watching a Run, so permission requests are answered by policy:

- **ACP:** `session/request_permission` is answered with the first
  `allow_once` option (or else `allow_always`).
- **Codex:** approval requests are approved.
- **Claude Code:** pass the permission mode in `command`, for example
  `["claude", "--permission-mode", "bypassPermissions"]`.

The container is the sandbox. Its limits (egress rules, no host access,
resource caps) are what keep an over-eager agent in bounds. Every answered
request is recorded as an event.

## Writing a workload for `generic`

Anything that reads stdin and writes stdout works. To be steerable, read
lines from stdin. To stop gracefully, handle SIGTERM within the grace
period (`workload.grace`, default 30s). To resume, keep your state under a
state volume and read it back on start, or set `workload.resume.command`.
