# Sybra

Local desktop app to orchestrate a swarm of Claude Code agents. Markdown-based task management, headless execution (`claude -p`, steerable mid-run), Wails v3 alpha GUI (darwin-only).

## Work-Data Confidentiality (HARD RULE)

Sybra is a public personal project. **Never** link, embed, paraphrase, or otherwise leak content from work repos (e.g. Kong / `konghq.*`) into the sybra repo or any artifact it produces.

Applies to:

- Issues and PRs opened against `Automaat/sybra` — manually, via Claude Code, or via any sybra automation
- Task bodies, decision logs, plan sidecars, commit messages, audit logs that may end up in PRs
- Auto-sources that ingest external content into tasks: GitHub Issues fetcher, Renovate fixer, orchestrator brain, review automations
- Logs, screenshots, and pasted snippets uploaded to issues/PRs

Forbidden content: work-org repo URLs, branch names, commit SHAs from work repos, ticket IDs (e.g. Jira keys), internal hostnames, customer names, code snippets from work repos.

### Enforcement mechanism

Two-layer defense applied to every automation that authors a sybra artifact for a work-typed task (`project.Type == work`):

1. **Semantic ceiling (prompt-level)** — review-agent prompts include explicit redaction rules naming the project's identifiers; agents are told to describe sybra bugs abstractly without quoting work content.
2. **Regex floor (`internal/scrub`)** — every agent- or detector-authored title/body is run through `scrub.Scrub(text, blocklist)` before persistence. Blocklist is derived from the project record (id, owner, repo, URL). Static patterns redact GitHub URLs, Jira-shaped keys, emails. Replacement is `[redacted]`.

Filing routes for work-typed tasks:

| Source | Public path (non-work) | Work-typed path |
|--------|------------------------|-----------------|
| Human review (`internal/sybra/app_human_review.go`) | GH issue on `Automaat/sybra` | Scrubbed local sybra task tagged `sybra-bug,scrubbed` (`fileLocalScrubbed`); origin task flipped to `blocked` with pointer to the local task |
| Monitor anomalies (`internal/sybra/monitor_sink.go`) | GH issue on `Monitor.IssueRepo` | Scrubbed local sybra task tagged `sybra-bug,scrubbed,monitor:<kind>` via `monitorRoutingSink` |
| LLM-dispatched anomalies | Agent files its own GH issue | Downgraded to deterministic path via `DowngradeLLMForTask` → goes through `monitorRoutingSink` (no agent invocation, no agent-authored content to leak) |

When adding a new auto-source that ingests external content or files artifacts on a public destination, route work-typed tasks through `App.workScrubContextForTask` + `scrub.Scrub` and create a local sybra task instead. Never paraphrase, summarise, or build a "lite" issue body that retains identifiers — the scrubber is the only sanctioned reduction.

## Project Structure

```
sybra/
├── main.go                  # Wails v3 bootstrap, embeds frontend/dist (darwin-gated)
├── main_other.go            # No-op stub for non-darwin
├── go.mod / go.sum
├── internal/
│   ├── agent/               # Provider runners, approvals, recovery, OS sandboxing
│   ├── artifact/            # Per-task artifact store (~/.sybra/artifacts/<task-id>/)
│   ├── config/              # YAML config, defaults, validation, docs generation
│   ├── github/              # Live GitHub integration surface (issues, PRs, checks, reviews)
│   ├── harnessevolution/    # CLI-driven proposal mining from persisted self-monitor reports
│   ├── selfmonitor/         # In-process report pipeline persisted for CLI/GUI reuse
│   ├── sybra/               # App wiring, Wails/HTTP services, orchestrator, automation loops
│   ├── task/                # Task model, parsing, manager/store, planning/review metadata
│   ├── workflow/            # Workflow engine, builtin YAMLs, verification/tamper/evidence gates
│   └── ...                  # ~80 internal packages total; inspect `internal/` directly
├── cmd/
│   ├── sybra-cli/           # Multi-command CLI (tasks, projects, PRs, selfmonitor, harness-evolution)
│   └── sybra-server/        # Headless HTTP/server entrypoint
├── .claude/
│   └── skills/              # Claude Code skills (auto-copied to ~/.sybra/skills on start)
│       ├── sybra-tasks.md # Task CRUD skill
│       └── sybra-triage.md # Triage workflow skill
├── tasks/                   # Markdown task files (runtime data)
├── frontend/
│   ├── src/
│   │   ├── App.svelte       # Root component
│   │   ├── main.ts          # Entry point
│   │   └── style.css
│   ├── bindings/            # Auto-generated Wails v3 bindings (TS, per-package)
│   └── package.json
└── build/                   # Build assets
```

## Tech Stack

### Backend

- **Go** (pinned version in `mise.toml`)
- **Wails v3 alpha** (pinned version in `go.mod`) — desktop app framework with service-based binding, multi-window, typed events. Darwin-only on this branch.
- **fsnotify** — file watching for task changes
- **gopkg.in/yaml.v3** — YAML frontmatter parsing

### Frontend

- **Svelte 5** + **TypeScript 6** (Vite 8)
- **Skeleton UI v4** (skeleton.dev) + Vox theme
- **Tailwind CSS v4**
- Auto-generated Wails v3 bindings in `frontend/bindings/` (per Go package, e.g. `internal/task/models`)
- `@wailsio/runtime` for IPC, events, browser/system APIs

### Tooling

- **mise** — tool version management (Go, Node — see `mise.toml`)
- **golangci-lint v2** — Go linting (gocritic, nilerr, nilnesserr, nilnil, nolintlint, modernize)
- **oxlint** — frontend linting
- **GitHub Actions** — CI (lint-go, lint-frontend, build)

## Architecture

### Extracting a Concern from `internal/sybra`

`internal/sybra` is a large package (App + 12 `svc_*.go` service structs). When
pulling a high-churn concern out of it (e.g. `internal/sybra/agentorch`,
`internal/sybra/review`), nest the new package under `internal/sybra/<concern>`
rather than promoting it to a top-level `internal/<domain>` package — this
keeps import-cycle-prone concerns (those that still need to reach back into
`App`-adjacent state) visually scoped to the god-package they were extracted
from. Only promote to top-level once a concern has zero remaining coupling
back into `internal/sybra`, the way `internal/recovery` already does (it
defines its own narrow interface instead of importing `internal/sybra`).

Keep the extracted type's exported surface narrow: private fields, a verb-only
public API, and `Set*` methods for any field that must be late-bound after
construction (see `agentorch.Orchestrator`'s `SetSandboxes`/`SetBgops`/
`SetConflictRecovery`). Reach for an accessor method only when an external
caller genuinely needs the underlying dependency itself (e.g. `Cfg()`,
`Worktrees()`) — never export the field directly.

### One UI Transport

Both builds reach the backend over HTTP. `internal/httpserve` assembles the handler — API dispatch, the SSE stream, health/metrics/pprof, and the SPA — and both binaries serve it: `cmd/sybra-server` on its configured bind, and the desktop app on `127.0.0.1:0`, with the Wails window opened at that origin rather than the `wails://` asset server. The desktop binary is therefore a server too; there is no in-process IPC path left.

`frontend/src/lib/api-http.ts` is the only implementation, and `frontend/src/lib/api.ts` re-exports it wholesale — never add a hand-written export list there, or a method has two places to be registered again.

**Adding a new bound method:**
1. Add the method to a service struct in `internal/sybra/svc_*.go` (or `App` itself).
2. Add it to the allowlist in `internal/sybra/services.go` (`ServiceRegistry`) — unlisted methods 404.
3. Add a `call(...)` wrapper in `frontend/src/lib/api-http.ts`. `scripts/check-api-shim-sync.sh` fails on a registry method with no wrapper.
4. Regenerate bindings: `wails3 generate bindings -ts -clean -d frontend/bindings ./...`. The frontend imports **types** from `frontend/bindings/`, never call paths; CI's `Wails Bindings Sync` job fails on drift.

A method whose first parameter is a `context.Context` gets it from the request — the JSON argument array carries only the remaining parameters.

**Local-only methods.** A method that acts on the host serving the board (open an editor or terminal, open a worktree, grab an OS hotkey, shell out to the claude CLI) is registered with `.WithLocalOnly(...)`. `httpapi` then serves it to loopback callers carrying no forwarding header, so the desktop window reaches it through its own server while a window attached to a board on another machine is refused.

**Events (Go → Frontend):**
- `agent:state:<id>` — agent state change
- `agent:output:<id>` — new StreamEvent from headless agent
- `task:updated` / `task:created` / `task:deleted` — file system changes

Emit via the App's `emit` closure, wired to `sse.Broker.Emit` in both binaries. The frontend subscribes with `EventsOn` from `$lib/api`, which multiplexes every subscription onto one `EventSource` against `GET /events`. `OnConnectionChange` reports that stream's health; `connectionStore` drives the offline banner from it and refetches the board when it comes back.

**Attaching to a board on another machine.** Set `SYBRA_SERVER_TARGET` (bare `host:port` or an `http(s)://` origin, same forms `sybra-cli` takes) plus `SYBRA_SERVER_TOKEN` before launching the desktop app. The bundle still comes from this process — only a page it served can be handed a bearer token — but every call and event goes to the named board, and **no local App starts**, so the laptop does not run a second orchestrator against its own home. An unresolvable target is a startup error, never a silent fall back to the local board. `BrowserService` stays local: opening a window or a link acts on the machine the operator is sitting at.

The desktop listener reuses the port recorded in `$SYBRA_HOME/desktop-port`. Browser storage is partitioned by origin **including port**, so a fresh port each launch would silently empty `localStorage` — colour scheme, open workspace tabs, pane sizes — on every start and every auto-update restart. That stable port is also what the attached board must list in its own `server.allowed_origins` (`http://127.0.0.1:<port>`), or its CORS check refuses every call the window makes.

The content-security policy comes from the response header `internal/httpserve` sets, and from nowhere else — never add a `<meta http-equiv="Content-Security-Policy">` back to `frontend/index.html`. A meta copy is a second source of truth that wins wherever it is stricter, which is how its `connect-src 'self'` blocked every call an attached window made no matter what the header allowed.

### Durable Storage Backend

`storage.database.backend` selects where durable state lives: `file` (the
default — the per-domain filesystem stores Sybra has always used), `sqlite`
(embedded single file, one machine alone), or `postgres` (shared server,
several machines on one board). Omitting the block changes nothing, so no
existing install needs migrating.

`internal/db` opens the handle, applies the embedded per-dialect migrations in
`internal/db/migrations/<dialect>/`, and is the only place that knows a dialect.
The query layer is deliberately plain `database/sql` with hand-written SQL and
`DB.Rebind` for placeholders — no ORM, no codegen. **Ask before changing that
choice**; every store written after this one depends on it.

Rules for a store that moves to the backend:

- Write SQL with `?` placeholders and let `Rebind` translate. A `?` inside a
  quoted literal is left alone and `??` escapes one literal `?` (postgres'
  jsonb key-existence operator).
- Cross the seam with integers, not engine types: `db.TimeValue`/`db.TimeFrom`
  for timestamps, `db.BoolValue`/`db.BoolFrom` for booleans. Stamp in-memory
  records with `db.StoredTime` — the wall clock is nanosecond-granular on
  Linux and stored timestamps keep microseconds, so a record returned straight
  from a write would never equal its own read-back.
- Wrap a read-modify-write in `DB.InTx` **and** take the row's write lock on
  the way in (`SELECT ... FOR UPDATE` on postgres; sqlite gets it from the
  DSN's `_txlock=immediate`). A plain read inside a transaction does not
  serialize: postgres loses the other writer's edit silently, sqlite fails with
  `SQLITE_BUSY_SNAPSHOT`.
- Never edit a migration that has shipped — the runner records a checksum and
  refuses to start on a changed file, because the edit reaches a fresh database
  and silently misses every existing one.
- Add the store's behaviour suite to `internal/testutil/dbtest.Each`/`Engines`
  so it runs on both engines, and to `scripts/test-db-engines.sh`'s package
  list so `mise run test:db` and CI cover it.

### Task Format

Tasks are YAML frontmatter + GFM markdown files in `tasks/`:

```yaml
---
id: task-abc123
title: Implement auth middleware
status: todo              # new|todo|in-progress|in-review|human-required|done
agent_mode: headless      # headless (legacy task files may still carry interactive; load-only, no longer dispatchable)
allowed_tools: []         # empty = all tools allowed
tags: [backend, auth]
project_id: owner/repo    # optional, links to a registered project
created_at: 2026-04-02T10:00:00Z
updated_at: 2026-04-02T10:00:00Z
---
## Description
Task body in markdown.
```

Parse with `task.Parse(path)` or `task.ParseBytes(data)`. Marshal with `task.Marshal(t)`.

### Projects

Projects mirror GitHub repos. Created from a GitHub URL, cloned as bare repos.

**Storage:** `~/.sybra/projects/` (YAML metadata), `~/.sybra/clones/` (bare git repos), `~/.sybra/worktrees/` (per-task checkouts).

**Flow:** Create project from URL → bare clone → assign `project_id` to tasks → agent start auto-creates worktree → worktree cleaned up on agent completion.

**Worktree base ref** (`worktree_base_ref`): controls the starting point for new worktree branches. `fresh` (default) branches off `origin/<default>` so worktrees always start from pushed remote state. `head` branches off the local HEAD of the default branch, picking up commits that exist locally but haven't been pushed yet. Empty value treated as `fresh` — existing projects require no migration. Configurable per-project from the Project → Setup tab.

**CLI:**
```bash
sybra-cli project list|get|create|delete
sybra-cli create --title "..." --project "owner/repo"
```

### Agent Execution Modes

**Headless** (`claude -p`):
```bash
claude -p --output-format stream-json [--resume <id>] [--allowedTools "..."]
```
- The prompt is piped over stdin (plain text, default `--input-format`), not a positional argument — argv is world-readable via `ps aux`/`/proc/PID/cmdline`, and a headless agent's own unscoped `pkill -f <pattern>` could otherwise self-match its own prompt text and kill itself. `runHeadlessAttemptPipe`/`startHeadlessSurviveProcess` write it and close stdin immediately (no steering).
- Go spawns process, reads stdout NDJSON line-by-line
- StreamEvent types: `init`, `assistant`, `tool_use`, `tool_result`, `result`
- Permission flags are a 4-case precedence (`claudePermissionArgs`, `internal/agent/provider_claude.go:117-135`):
  1. `len(allowed_tools) > 0` → `--allowedTools <list>` (explicit allowlist always wins, regardless of the other two settings)
  2. else `RequirePermissions` (`agent.require_permissions`, default `true`) → no bypass/auto flag; the approval hook gates each tool call
  3. else `HeadlessPermissionMode` (`agent.headless_permission_mode`) `== "auto"` → `--permission-mode auto` (auto-mode classifier)
  4. else → `--dangerously-skip-permissions` (legacy full bypass)

**Run guardrails — what the two ceilings actually measure.** Both are named in units they don't use, so read them as:

- `agent.max_cost_usd` **cannot pre-empt an overspend.** Providers report cost only on their terminal result event, so `Agent.CostUSD` is 0 for the whole run and the ceiling fires on a breach that is already paid for (runs have landed at $8.19 against a $5 limit). It is a circuit breaker against the *next* run, not a budget. `agent.max_task_cost_usd` is the cumulative-per-task cap and is checked before dispatch, so that one does gate.
- `agent.max_turns` **counts assistant stream events, not CLI turns.** `checkTurnsGuardrail` increments once per `assistant` event and one CLI turn emits several, so a configured `150` typically stops near a CLI-reported 80.

Codex and copilot report no USD at all (tokens and premium requests respectively). `stats.EstimateAgentCost` derives it so the ceilings can see them; `Agent.BankEstimatedCost` banks it live, and **must** run after `AddCacheStats` — cached input is ~95% of a codex run and prices at a tenth of standard, so estimating first overstates by ~6x. A provider-reported cost always wins over the estimate.

**Fork subagents** (`fork_subagent: true`): sets `CLAUDE_CODE_FORK_SUBAGENT=1` in the subprocess environment (CC v2.1.121+, claude provider only). Allows a single prompt to spawn parallel subagent runs, reducing wall-clock time for multi-part work. Tradeoff: each forked subagent incurs its own token usage — total cost multiplies with parallelism. Enable per-task from the metadata panel or task creation dialog. Not propagated to codex agents.

**Provider gate.** Headless dispatch resolves the provider through `prepareRunConfig` → `gateProvider` → `resolveProviderDecision`, so an unhealthy/rate-limited provider fails over (claude → codex → copilot) and the model is remapped (`NormalizeModel`). Failover is decided at *dispatch*; an agent already running when its own provider caps mid-run does not hot-swap (recovers by re-dispatch via `RescheduleRateLimitedAgent`).

**Interactive mode has been removed** (the persistent conversational-session runner, `agent_mode: interactive`). A running headless agent is instead steered mid-run over its stdin/stream-json transport (`agent.Manager.Run` with `HeadlessSteerable`) — see Steering below. `task.AgentModeInteractive` is kept only so pre-existing task files carrying it still parse; no path can mint a new one (`task.ValidateMintableAgentMode`), and no dispatch path honors it.

**Steering** a running headless agent: `SendMessage`/`GetConvoOutput` inject a mid-run message over the agent's stdin and stream the resulting `ConvoEvent`s back, gated on `Agent.CanSteer` (a live stdin transport — `agent.headless_steerable`, default `true`). This is the GUI's "steer agent" control on a running agent, not a second execution mode.

### Worktree Agent Context

Every implementation worktree is seeded with two git-excluded files (added to
`.git/info/exclude`, never committed):

- **`.sybra-context.md`** (`internal/worktree/context.go`) — identity beacon
  (task id, title, branch). Regenerated on every prepare.
- **`NOTES.md`** (`internal/notes` + `internal/worktree/notes.go`) — agent
  working-memory scratchpad: running plan, decisions, dead ends. Seeded
  **create-if-absent only** — `ensureNotesFile` must never clobber an existing
  file, since it is the agent's memory across runs/resume.

`agent.Manager.Run` is the single chokepoint that inlines `NOTES.md` (a
read/maintain instruction + the file's current contents, head+tail-capped) into
the prompt via `notes.SeedPrompt`. This is the resume substitute for Codex (no
`--resume`) and a context-rot-resistant scratchpad for all providers. The seed
lands on `Run`'s local `cfg` copy only, so the persisted `AgentRun.Prompt` stays
clean.

**Gated on `RunConfig.SeedWorkingMemory`, set only for code-author roles
(`Role.AuthorsCode`: implementation/fix-review/pr-fix).** Verifier roles
(review, test-runner, eval) reuse the *same* per-task worktree, so seeding them
would feed an independent reviewer/tester the implementer's notes — and since
`NOTES.md` is git-excluded, that bias is invisible to the diff-based
`detect_tampering` / `verify_checks` gates. Do not flip a verifier role to
seed.

`ensureNotesFile` fails closed: it git-excludes *before* creating the file and
aborts if exclusion fails, so an unignored scratchpad of work-derived notes is
never left for `SanitizeWorktree`'s `git add -A` to commit onto the PR. For
work-typed tasks the file stays local; route through `internal/scrub` if ever
summarized into a persisted artifact.

### OS-Level Process Sandbox

Env-level isolation (`SYBRA_HOME`, see above) is advisory — a malicious or
confused tool can still write anywhere the OS user can, which is the blast
radius the 2026-07-06 board-wipe incident (#1576) exploited. `internal/agent`
adds a second, OS-enforced layer: every provider CLI spawn (headless
pipe/survive, persistent claude convo, convo-survive, per-turn
codex/copilot) routes through one constructor, `newProviderCmd`
(`runner_core.go`). A `rg exec.CommandContext internal/agent` drift check must
only ever match `newProviderCmd` itself plus the documented non-provider probe
sites, so a new spawn site cannot obtain an unsandboxed provider process by
construction.

`newProviderCmd` wraps the invocation via `wrapInvocation`
(`procsandbox_darwin.go` / `procsandbox_linux.go`): `sandbox-exec` on darwin,
`bwrap` on Linux. In `enforce`, `Manager.injectProcessSandbox`
(`manager_run.go`) canonicalizes the write roots and only re-opens those for
mutation: the task worktree, sandbox home, `os.TempDir()`, the shared build
cache, provider CLI state dirs (`~/.claude`, `~/.codex`, `~/.copilot`), and
the toolchain cache (`~/.cache`). Reads stay unrestricted; the wrapper still
applies transitively to grandchildren.

Posture is `agent.sandbox_mode` (`off`/`report`/`enforce`, config default
**`report`**) plus a per-task `sandbox: false` escape hatch (nil/true =
inherit config; false = force off), resolved by
`agentorch.ResolveSandboxMode` and set into `RunConfig.SandboxMode`.
`Manager.injectProcessSandbox` (`manager_run.go`) resolves this once per run
into an unexported `RunConfig.sandbox` spec:

- `off`: no validation, no wrap.
- `report`: validates and logs the resolved allowlist via `slog`, but
  **never wraps the spawn** — a profile/SBPL defect can only ever affect an
  explicit `enforce` posture, never the default rollout posture. This is
  why `report` is safe to ship as the default.
- `enforce`: wraps the spawn and fails the run closed if the host sandbox
  mechanism or profile/setup is unavailable. It is **never** reached by
  leaving the key unset — the built-in default is `report`, so every
  deployment that wants containment must set `agent.sandbox_mode: enforce`
  explicitly. Under it the real operator board under `~/.sybra` and the
  deploy checkout `/opt/sybra/src` stay read-only to agents.

Do not record which posture a given deployment runs here — that claim drifts
silently and this section already carried a false one for months. Read it from
the host instead. The postures are distinguishable only by their log line and
by whether setup failures abort the run, never by observable agent behaviour on
a healthy host, so a config that quietly resolves to `report` looks exactly
like a contained one. Grep the app log (`~/.sybra/logs/sybra.log`, or
`/data/sybra/home/logs/sybra.log` on the server) — **not** the systemd journal,
which carries only build and start output:

- `agent.sandbox.enforce` — spawn wrapped.
- `agent.sandbox.report` — spawn **not** wrapped; allowlist logged only.
- `agent.sandbox.report.unavailable` — `report` that fell back to unwrapped
  because `bwrap`/`sandbox-exec` was missing.
- *neither line for a run* — resolved `off`, via config or the per-task
  `sandbox: false` escape hatch; `injectProcessSandbox` returns before it logs
  anything. Absence of both is the widest-blast-radius case, not the quiet one.

The escape hatch's use is operator-visible: `agentorch.logSandboxEscapeHatch`
logs a warning and records `audit.EventAgentSandboxDisabled`.

### Verified Experience Memory

Experience records under `~/.sybra/experience/` are advisory-only context for
triage and planning, never a deterministic decision gate. Work-typed project
records are scrubbed before they are written to disk; do not add a second raw
write path or surface experience content in public artifacts without going
through the same work-project scrub context.

### Per-Machine Automations

Sybra can run on multiple machines (e.g. laptop + remote server). Each instance has its own `~/.sybra/` and runs background automations independently. Two routing axes prevent duplicate work:

**1. Per-feature `enabled` toggle** (kill-switch per machine):
- `github.enabled` — GitHub Issues fetcher (`internal/sybra/app.go`)
- `umbrella.enabled` — auto-expand ☂️ umbrella issues into a gated task DAG (`internal/sybra/app_init.go` wires `umbrella.Expand` onto the issues fetcher)
- `renovate.enabled` — Renovate CI fixer (`internal/sybra/app_renovate.go`)
- Loop agents are stored per-machine in `~/.sybra/loop-agents/<id>.yaml` with their own `enabled` field — already independent.

**2. Top-level `project_types` allowlist** (per-project-type routing):
- Declares which `project.ProjectType` values this machine handles. Empty = all types.
- All project-scoped automations filter via `cfg.AllowsProjectType(...)` (config helper).
- Example: server handles `pet`, laptop handles `work`.

**3. `github.poller_role`** (shared-token de-dupe): `secondary` skips the periodic GitHub search polls (reviews/issues/renovate) so only the `primary` instance spends the shared token's rate budget. See `docs/github-rate-limits.md` for the full `github:` block (poll-interval overrides, GitHub App installation-token auth for a 15k/hr ceiling). Request volume is paced by `ghGate` and auto-throttled when the live budget (refreshed from the free `/rate_limit` endpoint) runs low.

```yaml
# server config
project_types: [pet]
github:   { enabled: true }
renovate: { enabled: true }
```

```yaml
# laptop config
project_types: [work]
github:   { enabled: true }
renovate: { enabled: true }
```

Startup logs an `app.automations` summary line so you can verify the role of each instance at a glance.

**Out of scope:** the orchestrator brain (`/sybra-monitor` Claude Code cron) is external to Sybra — manage it independently per machine via the Claude Code `schedule` skill.

### Server Deployment (home-nas)

Sybra runs headless as a **systemd service** (not Docker) directly on the LXC, deployed from `~/sideprojects/home-nas`. It builds from source on each start and **auto-deploys `main`** via the built-in `autoupdate` loop. Deployment artifacts live in this repo under `deploy/` (systemd unit + build/run scripts + full runbook in `deploy/README.md`).

- **Host:** `sybra` LXC (CT 114) on Proxmox, `192.168.20.219` (VLAN 20), Ubuntu 24.04, 6 cores / 16GB RAM — single-tenant (only Sybra runs here).
- **Runtime:** `systemd` unit `sybra` runs `sybra-server` directly (no container). `ExecStartPre=/opt/sybra/bin/sybra-build.sh` rebuilds the web bundle + Go binary from `/opt/sybra/src` (a git checkout on `main`) into `/opt/sybra/build`; `ExecStart` runs it via `mise exec`. Toolchain (go/node) + `claude`/`codex` CLIs are host-installed via `mise` for the `sybra` user.
- **Data:** `/data/sybra/home` ⇄ `~sybra/.sybra` (symlink), `/data/sybra/{claude,codex,klaudiush}` ⇄ the sybra user's `~/.claude`, `~/.codex`, `~/.config/klaudiush`. Tasks, config, projects, worktrees, and the agent registry live under `/data/sybra/home` and survive any restart/redeploy.
- **Lossless redeploy:** the unit sets `KillMode=process`, so a restart signals only `sybra-server`; the detached (`setsid`) agent subprocesses keep running and are re-adopted by `ReattachAll` on the next start (no interrupted turn). `Restart=on-failure` + `RestartForceExitStatus=42` + `TimeoutStopSec=45` mean a crash or a hung shutdown always self-recovers.
- **Auto-deploy:** `auto_update` (config `enabled: true`, `mode: auto`, `repo_dir: /opt/sybra/src`) polls `origin/main` every 5 min, requires configured `required_checks` to be green, `git merge --ff-only`s the approved SHA, then requests a restart (exit 42) → `ExecStartPre` rebuilds → lossless restart. Restarts are coalesced by `auto_update.coalesce_seconds` (default 1h), so bursts of green merges do not flap the service. `sybra-build.sh` keeps the last-good build on a failed candidate, so a broken build never downs the service.
- **Exposure:** local `:8080` → Traefik → `synapse.mskalski.dev` (Cloudflare DNS+TLS). ACL-locked to LAN, Cloudflare Tunnel, Tailscale CIDRs.
- **Deploy:** `ansible/playbooks/setup-sybra-lxc.yml` (provision LXC), `ansible/playbooks/deploy-sybra.yml` (provision toolchain, install unit + scripts, render config, `systemctl restart`).
- **Klaudiush hooks:** enabled in both Claude Code `settings.json` and Codex `config.toml` (`codex_hooks = true`) for event monitoring.

**SSH access:** `ssh root@192.168.20.219` (no DNS for `synapse`/`synapse.mskalski.dev` from outside LAN — use IP). Inventory: `home-nas/ansible/inventory.yml` → group `sybra_lxc`. Common debug commands:

```bash
ssh root@192.168.20.219 "systemctl status sybra"                     # service status
ssh root@192.168.20.219 "journalctl -u sybra -n 100 --no-pager"      # unit journal (build + start)
ssh root@192.168.20.219 "tail -100 /data/sybra/home/logs/sybra.log"  # sybra-server app logs
ssh root@192.168.20.219 "ls /data/sybra/home/tasks/"                 # task files
ssh root@192.168.20.219 "sudo -u sybra /opt/sybra/build/sybra-cli list"      # built CLI from the active candidate
```

Deploying = merge to `main` (auto-deploy polls within ~5 min, then restarts once CI-green + coalesce gates allow it) or `systemctl restart sybra` (rebuilds current `/opt/sybra/src` HEAD). To pin/rollback: `git -C /opt/sybra/src checkout <sha>` then restart (autoupdate's ff-only check pauses while off `main`).

**Toolchain on the server host.** The LXC has `mise` (+ go/node and the `claude`/`codex` CLIs) installed for the `sybra` user, but no per-project language tools. Every project declares its own bootstrap, resolved from two layers per worktree:

1. `setup:` in the repo's `.sybra.yaml` — canonical toolchain, checked into git, identical on every machine.
2. `SetupCommands []string` in `~/.sybra/projects/<id>.yaml` — machine-local extras (optional), editable from the Project → Setup tab in the UI.

Commands from (1) run first, then (2). Sybra executes them in the worktree root with a 5-minute batch timeout; stdout/stderr stream to `~/.sybra/logs/worktrees/<task-id>-setup.log`. A non-zero exit aborts worktree creation (the agent never starts on a broken toolchain). The merge lives in `internal/project.MergeSetup` and is tested in `TestPrepareForTask_MergesRepoAndAppSetup`.

Typical repo `setup:` examples:

- Go/Node projects (e.g. synapse itself): `mise install` + `(cd frontend && npm ci)`
- pure npm: `npm ci`
- uv/Python: `uv sync`
- multi-step: `./.sybra/bootstrap.sh`

Repo `.sybra.yaml` may also declare `checks.codegen` for the deterministic
mutation pass run by `simple-task-implement` before tamper/verify/review/
testing. Keep this list to formatters / codegen refresh only (for example
`golangci-lint fmt ./...`, `go mod tidy`), not tests; tests belong in
`checks.verify`.

App-level `SetupCommands` should stay empty for most projects; use it only for host-specific extras such as copying a local `.env`.

**Server-context quality gates.** On the server, do NOT treat the desktop build (`go build .`) as a commit gate — webkit2gtk is not installed and desktop builds are a CI concern (and darwin-only). Use `mise run build:server` (HTTP server) or `go build ./cmd/sybra-server` for a server-side build verification instead. Lint (`golangci-lint run ./...`, `hadolint Dockerfile`, `npx oxlint`) and tests (`go test ./...`) remain the authoritative gates — all installable via the project's `mise install` bootstrap.

## Development Workflow

### Running Locally

```bash
mise run dev          # rebuild frontend + go run . — desktop app on Wails v3 (darwin-only)
```

There is no Vite-backed hot reload — the frontend is built once per `mise run dev` invocation, the desktop binary serves the built bundle. Restart the task to pick up frontend changes.

### Adding a Backend Feature

1. Add/modify Go types in `internal/<package>/`.
2. If exposing to frontend: add a method to a service struct in `internal/sybra/svc_*.go` (or to `App`), then follow **One UI Transport** above — allowlist it in `internal/sybra/services.go`, add its `call(...)` wrapper in `frontend/src/lib/api-http.ts`, and regenerate bindings for the types.

### Adding a Frontend Feature

1. Create/edit Svelte component in `frontend/src/`.
2. Use Skeleton UI components from `@skeletonlabs/skeleton-svelte`.
3. Call Go backend through `$lib/api` (do NOT import from `frontend/bindings/` directly in components).
4. Listen for events with `EventsOn("event:name", callback)` from `$lib/api`.

### Testing

- Go: `go test ./...` — this does **not** compile or run the e2e suite (see below).
- E2E: `go test -race -tags e2e -timeout 10m ./internal/sybra/...` — matches the CI `test-go-e2e` job exactly; 8 files behind `//go:build e2e`. Add `-short` for a ~45s smoke pass that skips the slowest retry-backoff and chaos tests.
- Use table-driven tests for Go packages
- Frontend: `cd frontend && npm run check` (svelte-check)
- Manual runtime smoke tests: see `docs/manual-testing.md` for the isolated
  `SYBRA_HOME` + fake-provider CLI harness that exercises the real HTTP server,
  workflows, agent runners, stats, audit, and Evaluation report without
  touching local user data or spending model credits.
- Never guard a git-dependent test with a per-test `t.Skip("git not
  available")` — a broken environment then reports green while silently
  dropping coverage. `internal/project`, `internal/worktree`, and
  `internal/sybra` each enforce this via a package-level `TestMain` that
  `os.Exit(1)`s immediately if `git` is missing from `PATH`; add new
  git-dependent test packages to that pattern instead of a local skip.

## Quality Gates

**`mise run verify` is the pre-commit gate — it runs every deterministic, CI-aligned gate in `.github/workflows/ci.yml`** (frontend build:desktop + build:web, `go build ./...`, `go mod verify`, `go mod tidy` drift check, `go test -race ./...`, `go test -race -tags e2e ./internal/sybra/...`, golangci-lint, frontend check + test:coverage + oxlint + pin-strategy, api-shim sync, no-home-fallback gate, Wails bindings drift check, hadolint). "Deterministic" means the outcome depends only on repo state, not ambient CI infra — some steps (`npm ci`, Go module resolution) still need network access. It intentionally excludes the CI jobs that need external advisory DBs, a browser, or a container runtime and so can't run as a reliable pre-commit loop — `lint-nilaway`, `security` (govulncheck + npm audit), the Playwright `e2e` job, and `test-go-db` (run `mise run test:db` by hand before touching SQL); CI stays the source of truth for those four. Running only `go test ./...` skips the e2e suite entirely (it's gated behind `//go:build e2e`) and will ship green-local / red-CI.

```bash
mise run verify
```

Before committing:

- [ ] `mise run verify` passes
- [ ] `cd frontend && npm run build:desktop && cd .. && go build .` succeeds (darwin)

```bash
# Lint all
mise run lint

# Go tests (excludes e2e — see Testing above)
go test ./...

# Frontend type-check
cd frontend && npm run check

# Full build
mise run build
```

## Common Commands

```bash
# Dev server with hot reload
mise run dev

# Build production binary
mise run build

# Lint everything (Go + frontend)
mise run lint

# Go lint only
golangci-lint run ./...

# Frontend lint only
cd frontend && npx oxlint .

# Frontend type-check
cd frontend && npm run check

# Go tests
go test ./...

# Install frontend deps
cd frontend && npm install
```

## CLI (`sybra-cli`)

Standalone binary for task CRUD, used by Claude Code skills. Installed via `go install ./cmd/sybra-cli`.

```bash
sybra-cli [--json] <command> [flags]

list     [--status STATUS] [--tag TAG]
get      <id>
create   --title TITLE [--body BODY] [--mode MODE] [--tags t1,t2]
update   <id> [--title T] [--status S] [--body B] [--mode M] [--tags T]
delete   <id>
config   dump | doctor
```

- `--json` for machine-parseable output (used by skills)
- `mise run dev` auto-installs latest CLI before launching the desktop app
- `config dump` prints the resolved `~/.sybra/config.yaml` (env overrides applied, `server.auth_token` redacted); `config doctor` sanity-checks data dirs, `agent.provider`, `agent.headless_permission_mode`, and enabled integrations missing required credentials. See `docs/CONFIG.md` (generated from `internal/config` struct tags via `go generate ./internal/config/...`) for the full key reference.

**The CLI is a client and nothing else.** It never opens the board's files — that made it a second writer behind whichever instance owned them, and a stale target turned an ordinary edit into a change the owner later overwrote. Every command that touches board state resolves a server and **refuses** when none answers, naming it.

Target resolution, all loopback so the token never crosses a network hop:

1. `SYBRA_SERVER_TARGET` (+ `SYBRA_SERVER_TOKEN` for a board on another machine)
2. else the port the desktop app recorded in `$SYBRA_HOME/desktop-port`
3. else the configured server port

`SYBRA_HOST`, `SYBRA_PORT`, and `SYBRA_BIND_ADDR` are deliberately **not** consulted. They name where a server should *listen*; letting one steer a client aims it — and the bearer token it sends next — at whatever answers there. Read `cfg.Cluster` directly, never `ListenAddrs`, which honours `SYBRA_BIND_ADDR` internally.

`--home` / `SYBRA_CONTROL_HOME` / `SYBRA_HOME` select **which board's** config and recorded port to read — they no longer mean "edit files instead". `config`, `health`, `install-skills`, and `cluster` (its `gen-cert`/`nodes` halves touch no board; `reassign` refuses for itself) still run with no server. So does `doctor`, because it is what an operator reaches for when the server is what broke, but it refuses to delete.

**A board is trusted with this disk only when it serves this home.** `GET /health` reports the `home` an instance serves, and the CLI compares it against `config.HomeDir()`. Loopback is not the question: two instances on one machine are both loopback and own different homes, and an address-only check let a cleanup delete a live sandbox belonging to the other one. Anything that acts on local files must ask `apiClient.ownsHome`, never `!remote`.

A contended record comes back as `503` and the CLI exits **75**, which is what an agent retries on. Never map it to a plain failure — the agent then abandons work it only had to repeat.

Tests get a board, not a directory: `startTestBoard` (`cmd/sybra-cli/testboard_test.go`) mounts the real stores on the real dispatcher, so a test still asserts against files on disk and a missing task is still a 404. Add a service there when a command starts calling a new one.

### Skills

Project-local Claude Code skills in `.claude/skills/`:
- `sybra-tasks.md` — task CRUD via CLI (`/sybra-tasks`)
- `sybra-triage.md` — triage workflow (`/sybra-triage`)

Skills are auto-copied to `~/.sybra/skills/` on app startup (via `syncSkills()` in `app.go`).

### Orchestrator Brain

`orchestrator/CLAUDE.md` — system instructions for Claude Code orchestrator sessions. Copied to `~/.sybra/CLAUDE.md` on app start. Covers: triage rules, dispatch logic, monitoring, failure handling, escalation criteria.

## Build Order

Frontend must build before Go compilation due to `//go:embed all:frontend/dist`:

1. `cd frontend && npm install && npm run build:desktop` → produces `frontend/dist/`
2. `go build .` (darwin) — embeds `frontend/dist/` into the desktop binary

`mise run dev` and `mise run build` handle this sequencing automatically. Manual `go build .` requires step 1 first.

## Anti-Patterns

**AVOID:**

- ❌ Running `go build .` without building the frontend first — `//go:embed` fails if `frontend/dist/` is missing
- ❌ Forgetting to regenerate v3 bindings after adding/changing service methods (run `wails3 generate bindings -ts -clean -d frontend/bindings ./...`); CI's `Wails Bindings Sync` job catches drift
- ❌ Editing files in `frontend/bindings/` — these are auto-generated and get overwritten
- ❌ Calling into `frontend/bindings/` from components/stores — those are generated Wails call paths the UI no longer uses; import calls from `$lib/api` and take only **types** from `frontend/bindings/`
- ❌ Adding a hand-written export to `frontend/src/lib/api.ts` — it re-exports `api-http.ts` wholesale so a method is registered in one place; `scripts/check-api-shim-sync.sh` fails on a list there
- ❌ Exposing a method that opens a GUI app, grabs an OS hotkey, or shells out on the serving host without `.WithLocalOnly(...)` — a board reached from another machine would run it on the host serving it
- ❌ Storing agent state in files — agents are in-memory only, tasks are file-backed
- ❌ Using `allowed_tools: []` without understanding the fallback is governed by `agent.require_permissions`/`agent.headless_permission_mode`, not always `--dangerously-skip-permissions` — see the permission-flag precedence under Agent Execution Modes
- ❌ Adding a new auto-task source without (a) an `Enabled bool` toggle in its config block and (b) `cfg.AllowsProjectType(...)` filtering if the source is project-scoped — both are required so users running Sybra on multiple machines can route work without duplication
- ❌ Adding a new pipeline status/stage without a matching handoff entry point — every stage a task can sit at must be directly reachable via `sybra-cli handoff --stage <name>`. That means: add the stage to `handoffStageRegistry` (`cmd/sybra-cli/main.go`) mapping it to the right `handoff*` tags, and add a matching variant to the single handoff template at `internal/workflow/builtin/simple-task-handoff.yaml` so a fresh task flips straight to that status while adopting its `worktree_dir`. Without this you cannot inject a task at the new stage to test/demo it in isolation (e.g. `--stage testing` → generated `simple-task-handoff-testing` → status `testing`)
- ❌ Baking project toolchains into the server host or `Dockerfile` — the server host has `mise` only (the darwin `Dockerfile` is CI/legacy, not the deploy path). Language-specific tools belong in each project's **Setup commands** (see Server Deployment section). New projects in new languages never require any host/image change.
- ❌ Treating `go build .` (desktop) as a server-context commit gate — Wails v3 needs GTK/webkit on Linux (not installed server-side) and desktop is darwin-only/CI-owned. Use `mise run build:server` for server-side verification.
- ❌ Pasting, linking, or paraphrasing work-repo content (URLs, branches, SHAs, ticket IDs, snippets, logs, customer names) into sybra issues/PRs/tasks/commits — see **Work-Data Confidentiality** at the top. Any new auto-source that ingests external content must filter work-repo content at the source, not in post-processing.
- ❌ Surfacing `internal/artifact/` content in a GitHub issue/PR/comment without routing through `App.workScrubContextForTask` + `scrub.Scrub` first — the artifact store is raw/local-debug-only and never scrubs at write time.
- ❌ Reconstructing the operator's real Sybra home (joining a home-dir lookup with `.sybra`) anywhere outside `config.HomeDir()` (`internal/config/config_defaults.go`) — this is the exact pattern that caused the 2026-07-06 board wipe (#1576: a test suite silently fell back to `~/.sybra` and deleted files there). Call `config.HomeDir()` (Go) or require `SYBRA_HOME` explicitly (shell/TS) instead. `scripts/check-no-home-fallback.sh` (run in `mise run verify` and CI) fails on new occurrences outside its allowlist.
