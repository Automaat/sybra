# ADR: codex app-server stdio vs codex exec --json NDJSON

**Status:** Accepted  
**Date:** 2026-06-25  
**Issue:** #1031 (supersedes #704)  
**Spike branch:** `chore/spike-codex-app-server-stdio-vs-ndjson-435246db`

---

## Context

Sybra drives Codex via `codex exec --json` and hand-rolls NDJSON parsing in
`internal/agent/stream.go:178-300`. The parsed event types (`thread.started`,
`turn.completed`, `item.completed`) map to Sybra's `CodexEvent` /
`ConvoEvent` types.

`codex app-server` (shipped in 0.136, stable in 0.142.2) exposes the same
agent loop over a JSON-RPC 2.0 protocol (stdio or Unix socket). It is what
the new `openai-codex` Python SDK and the VSCode extension use. The question
is: should Sybra adopt it?

---

## Spike

`cmd/codex-appserver-spike/main.go` was run against `codex 0.142.2`. The spike
exercised the full handshake (`initialize` → `thread/start` → `turn/start`)
and captured one complete turn ("Reply with exactly: hello from app-server").

### Observed protocol flow

```
client → initialize
server ← response: userAgent, codexHome, platformFamily
server ← remoteControl/status/changed
client → thread/start  {cwd}
server ← response: thread{id, path, model, sandbox, approvalPolicy, …}
server ← thread/started
server ← mcpServer/startupStatus/updated  ×2 (starting → ready)
client → turn/start  {threadId, input}
server ← response: turn{id, status=inProgress}
server ← thread/status/changed {type: active}
server ← turn/started
server ← hook/started + hook/completed  (sessionStart hook from user config)
server ← hook/started + hook/completed  (userPromptSubmit hook from user config)
server ← item/started + item/completed  (userMessage)
server ← item/started                   (reasoning)
server ← item/completed                 (reasoning)
server ← item/started                   (agentMessage)
server ← item/agentMessage/delta ×4     ("hello", " from", " app", "-server")
server ← item/completed                 (agentMessage{text: "hello from app-server"})
server ← thread/tokenUsage/updated
server ← account/rateLimits/updated
server ← hook/started + hook/completed  (stop hook from user config)
server ← thread/status/changed {type: idle}
server ← turn/completed {durationMs: 4942}
```

Total wall-clock: 7.6 s (including process startup and MCP init).

### Token data (thread/tokenUsage/updated)

| field                  | value  | available in NDJSON? |
|------------------------|--------|----------------------|
| inputTokens            | 16 792 | ✅                    |
| cachedInputTokens      | 4 992  | ✅                    |
| outputTokens           | 27     | ✅                    |
| reasoningOutputTokens  | 17     | ✅                    |
| totalTokens            | 16 819 | ❌ (computed by Sybra) |
| modelContextWindow     | 258 400| ❌ (not in NDJSON)    |
| last vs total split    | yes    | ❌ (no multi-turn sum) |

---

## Comparison

### Event richness

| capability                     | codex exec --json    | app-server --stdio             |
|--------------------------------|----------------------|--------------------------------|
| streaming text deltas          | ❌                   | ✅ item/agentMessage/delta      |
| explicit reasoning items       | ❌                   | ✅ type=reasoning               |
| turn durationMs                | ❌                   | ✅ turn.completed               |
| multi-turn token accumulation  | ❌ (per-run only)    | ✅ last + total                 |
| modelContextWindow             | ❌                   | ✅                              |
| hook visibility                | ❌                   | ✅ hook/started + completed     |
| MCP server status              | ❌                   | ✅ mcpServer/startupStatus      |

### Process model

| aspect                      | codex exec --json               | app-server --stdio                  |
|-----------------------------|---------------------------------|-------------------------------------|
| process lifetime            | one subprocess per turn         | persistent daemon process           |
| per-turn startup cost       | ~0.5 – 1 s                      | ~0 (process already running)        |
| isolation between tasks     | process-level (strongest)       | thread-level (weaker)               |
| config isolation flags      | `--ignore-user-config --ignore-rules` | ❌ no per-thread equivalent    |
| user hooks                  | suppressed via --ignore-rules   | ✅ always fire (observed in spike)   |
| resume across restarts      | `codex exec --resume <session>` | `thread/resume` (richer)            |
| protocol complexity         | one-shot command + NDJSON       | JSON-RPC 2.0 handshake (3 round trips) |

### Config isolation: the key blocker

Sybra's current invocation for both headless and conversational agents:

```go
args := []string{"exec", "--json", "--skip-git-repo-check",
    "--ignore-user-config", "--ignore-rules"}
```

These flags prevent:
- `~/.codex/config.toml` (model, sandbox, MCP servers) from affecting the agent
- `AGENTS.md` files in the working tree from being injected as system prompt
- User hooks from firing (`sessionStart`, `userPromptSubmit`, `stop`)

`codex app-server` exposes process-level `-c/--config` overrides that can
point the daemon at a custom config file. However, the spike identified two
gaps that have no per-thread equivalent in `ThreadStartParams`:

1. **Repo rules / instruction sources** — `AGENTS.md` files in the working
   tree are loaded as system-prompt injections. The spike observed
   `instructionSources` in the server's `initialize` response; there is no
   per-thread flag to suppress them.
2. **Hook and MCP isolation** — user hooks (`sessionStart`, `userPromptSubmit`,
   `stop`) fire on every turn (3 hooks, ~150 ms overhead observed), and user
   MCP servers are started at daemon boot and shared across all threads.

`ThreadStartParams` allows per-thread `sandbox`, `model`, and `approvalPolicy`
overrides, but none of these cover hook suppression or instruction-source
exclusion. A `-c/--config` daemon restart with a stripped config would
suppress hooks globally, but Sybra needs this isolation per-task without
restarting the shared daemon.

For Sybra's multi-task orchestration these two gaps are concrete problems:
- User's `stop` hook might report telemetry or send notifications Sybra doesn't want
- User's `userPromptSubmit` hook might transform prompts in unexpected ways
- User's MCP servers (chrome-devtools, etc.) are started even for isolated agents
- `AGENTS.md` from one project's worktree could bleed into another thread's context

---

## Decision

**Do not adopt `codex app-server --stdio` for Sybra today.**

Keep `codex exec --json` with `--ignore-user-config --ignore-rules` for both
headless and conversational modes.

### Rationale

The config isolation gap is a correctness issue, not a performance issue. If
user hooks fire on every Sybra-managed turn, agent behaviour becomes
non-deterministic across machines (each user has a different hooks.json). The
`--ignore-user-config`/`--ignore-rules` flags exist precisely because Sybra
needs agents with predictable, controlled environments.

The event-richness benefits (streaming deltas, reasoning items, durationMs)
are UX improvements, not blockers. The per-turn spawn overhead (~0.5 s) is
manageable at current agent volumes.

### What to revisit when

Switch to app-server for **conversational mode** when codex ships:

1. A per-thread mechanism to suppress hook loading (e.g. `hooks: { enabled: false }`
   in `ThreadStartParams`) — the spike confirmed hooks fire unconditionally today.
2. A per-thread mechanism to exclude repo instruction sources / `AGENTS.md` injection
   (a `--ignore-rules` equivalent scoped to the thread, not the daemon).
3. Confirmed equivalence between `sandbox: "workspace-write"` and
   `codex exec --sandbox workspace-write --ignore-user-config`.

At that point, conversational mode benefits most: streaming text deltas
(`item/agentMessage/delta`) would enable real-time chat UI without
per-character polling, and the persistent process eliminates the ~1 s
per-turn respawn overhead.

Headless mode can stay on `codex exec --json` indefinitely — the one-shot
subprocess model provides the strongest isolation and `--resume` works well.

### Supersedes #704

\#704 proposed `codex remote-control`, which is now daemon + server-token
oriented at remote machines — wrong fit for Sybra's local subprocess model.
This spike evaluates the correct structured-protocol alternative (`app-server`).
Close #704 as superseded by this ADR.

---

## Consequences

- `internal/agent/stream.go` NDJSON parsing is retained as-is.
- `cmd/codex-appserver-spike/` is kept in the repo as a runnable reference
  for the protocol (see `go run ./cmd/codex-appserver-spike/`).
- `docs/codex-setup.md` may note app-server as a future direction.
- Revisit this ADR when codex adds per-thread hook suppression.
