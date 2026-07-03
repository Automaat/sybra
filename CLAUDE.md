# Sybra

Local desktop app to orchestrate a swarm of Claude Code agents. Markdown-based task management, two execution modes (interactive tmux + headless `claude -p`), Wails v3 alpha GUI (darwin-only).

## Work-Data Confidentiality (HARD RULE)

Sybra is a public personal project. **Never** link, embed, paraphrase, or otherwise leak content from work repos (e.g. Kong / `konghq.*`) into the sybra repo or any artifact it produces.

Applies to:

- Issues and PRs opened against `Automaat/sybra` — manually, via Claude Code, or via any sybra automation
- Task bodies, decision logs, plan sidecars, commit messages, audit logs that may end up in PRs
- Auto-sources that ingest external content into tasks: Todoist polling, GitHub Issues fetcher, Renovate fixer, orchestrator brain, review automations
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
│   ├── task/                # YAML frontmatter + markdown task CRUD
│   │   ├── model.go         # Task struct, Status enum
│   │   ├── parser.go        # Frontmatter parse/marshal
│   │   └── store.go         # Filesystem-backed store
│   ├── artifact/            # Per-task harness artifact store (~/.sybra/artifacts/<task-id>/)
│   │   ├── model.go         # Meta struct, Kind enum, Artifact write request
│   │   └── store.go         # Put/Append/List/Read/Delete/Reindex
│   ├── agent/               # Agent lifecycle management
│   │   ├── model.go         # Agent struct, State enum, StreamEvent
│   │   ├── manager.go       # Start/stop/list agents
│   │   └── runner_headless.go # claude -p NDJSON stream parser
│   ├── tmux/
│   │   └── manager.go       # tmux session CRUD via os/exec
│   ├── project/             # GitHub repo mirror + git worktree management
│   │   ├── model.go         # Project struct
│   │   ├── store.go         # YAML-backed project store
│   │   └── git.go           # Clone, worktree, fetch operations
│   ├── watcher/
│   │   └── watcher.go       # fsnotify on tasks/ dir, debounced
│   └── github/
│       └── interface.go     # Future: GitHub issue sync interface
├── cmd/
│   └── sybra-cli/         # CLI for task CRUD (used by Claude Code skills)
│       └── main.go
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

- **Go 1.26.4**
- **Wails v3 alpha** (`v3.0.0-alpha2.106`, per go.mod) — desktop app framework with service-based binding, multi-window, typed events. Darwin-only on this branch.
- **fsnotify** — file watching for task changes
- **gopkg.in/yaml.v3** — YAML frontmatter parsing

### Frontend

- **Svelte 5** + **TypeScript 6** (Vite 8)
- **Skeleton UI v4** (skeleton.dev) + Vox theme
- **Tailwind CSS v4**
- Auto-generated Wails v3 bindings in `frontend/bindings/` (per Go package, e.g. `internal/task/models`)
- `@wailsio/runtime` for IPC, events, browser/system APIs

### Tooling

- **mise** — tool version management (Go 1.26.4, Node 24)
- **golangci-lint v2** — Go linting (gocritic, nilerr, nilnesserr, nilnil, nolintlint, modernize)
- **oxlint** — frontend linting
- **GitHub Actions** — CI (lint-go, lint-frontend, build)

## Architecture

### Wails v3 Binding Convention

The `App` struct and the 12 service structs (`internal/sybra/svc_*.go`) are registered via `App.V3Services()` and exposed to the frontend through `wails3 generate bindings`. Bindings live under `frontend/bindings/` keyed by Go package path (e.g. `frontend/bindings/github.com/Automaat/sybra/internal/sybra/taskservice.ts`).

**Adding a new bound method:**
1. Add method to a service struct in `internal/sybra/svc_*.go` (or `App` itself).
2. Regenerate bindings: `wails3 generate bindings -ts -clean -d frontend/bindings ./...`.
3. Re-export from `frontend/src/lib/api.ts` so the rest of the frontend hits the shim, not the binding directly.
4. CI's `Wails Bindings Sync` job runs the same generate command and fails on drift.

**Wails events (Go → Frontend):**
- `agent:state:<id>` — agent state change
- `agent:output:<id>` — new StreamEvent from headless agent
- `task:updated` / `task:created` / `task:deleted` — file system changes

Emit events via the App's `emit` closure (set up in `main.go` to wrap `app.Event.Emit`). The frontend subscribes via `EventsOn` from `$lib/api`, which adapts v3's `WailsEvent` to the variadic callback shape stores expect.

### Task Format

Tasks are YAML frontmatter + GFM markdown files in `tasks/`:

```yaml
---
id: task-abc123
title: Implement auth middleware
status: todo              # new|todo|in-progress|in-review|human-required|done
agent_mode: headless      # interactive|headless
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
claude -p "prompt" --output-format stream-json [--resume <id>] [--allowedTools "..."]
```
- Go spawns process, reads stdout NDJSON line-by-line
- StreamEvent types: `init`, `assistant`, `tool_use`, `tool_result`, `result`
- Permission flags are a 4-case precedence (`claudePermissionArgs`, `internal/agent/provider_claude.go:117-135`):
  1. `len(allowed_tools) > 0` → `--allowedTools <list>` (explicit allowlist always wins, regardless of the other two settings)
  2. else `RequirePermissions` (`agent.require_permissions`, default `true`) → no bypass/auto flag; the approval hook gates each tool call
  3. else `HeadlessPermissionMode` (`agent.headless_permission_mode`) `== "auto"` → `--permission-mode auto` (auto-mode classifier)
  4. else → `--dangerously-skip-permissions` (legacy full bypass)

**Fork subagents** (`fork_subagent: true`): sets `CLAUDE_CODE_FORK_SUBAGENT=1` in the subprocess environment (CC v2.1.121+, claude provider only). Allows a single prompt to spawn parallel subagent runs, reducing wall-clock time for multi-part work. Tradeoff: each forked subagent incurs its own token usage — total cost multiplies with parallelism. Enable per-task from the metadata panel or task creation dialog. Not propagated to interactive or codex agents.

**Interactive** (tmux):
```bash
tmux new-session -d -s sybra-<id> -x 200 -y 50 "claude"
```
- GUI polls `tmux capture-pane -t sybra-<id> -p` for preview
- User attaches via terminal

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

### Verified Experience Memory

Experience records under `~/.sybra/experience/` are advisory-only context for
triage and planning, never a deterministic decision gate. Work-typed project
records are scrubbed before they are written to disk; do not add a second raw
write path or surface experience content in public artifacts without going
through the same work-project scrub context.

### Per-Machine Automations

Sybra can run on multiple machines (e.g. laptop + remote server). Each instance has its own `~/.sybra/` and runs background automations independently. Two routing axes prevent duplicate work:

**1. Per-feature `enabled` toggle** (kill-switch per machine):
- `todoist.enabled` — Todoist polling (`internal/sybra/app_todoist.go`)
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
todoist:  { enabled: true, api_token: ... }
github:   { enabled: true }
renovate: { enabled: true }
```

```yaml
# laptop config
project_types: [work]
todoist:  { enabled: false }
github:   { enabled: true }
renovate: { enabled: true }
```

Startup logs an `app.automations` summary line so you can verify the role of each instance at a glance.

**Out of scope:** the orchestrator brain (`/sybra-monitor` Claude Code cron) is external to Sybra — manage it independently per machine via the Claude Code `schedule` skill.

### Server Deployment (home-nas)

Sybra also runs headless as a server, deployed from `~/sideprojects/home-nas`.

- **Host:** `synapse` LXC (CT 114) on Proxmox, `192.168.20.219` (VLAN 20), Ubuntu 24.04, 6 cores / 16GB RAM
- **Container:** `ghcr.io/automaat/sybra:<version>` via Docker Compose (container name: `sybra`)
- **Compose file:** `/opt/synapse/docker-compose.yml` on host (source: `ansible/docker-compose/synapse-stack.yml`)
- **Volumes:** `/data/sybra/home` (→ `~/.sybra` inside container), `/data/sybra/claude` (Claude Code settings + hooks), `/data/sybra/codex` (Codex config)
- **Exposure:** local `:8080` → Traefik → `synapse.mskalski.dev` (Cloudflare DNS+TLS). ACL-locked to LAN, Cloudflare Tunnel, Tailscale CIDRs.
- **Deploy:** `ansible/playbooks/setup-synapse-lxc.yml` (provision LXC), `ansible/playbooks/deploy-synapse.yml` (push compose + restart)
- **Klaudiush hooks:** enabled in both Claude Code `settings.json` and Codex `config.toml` (`codex_hooks = true`) for event monitoring

**SSH access:** `ssh root@192.168.20.219` (no DNS for `synapse`/`synapse.mskalski.dev` from outside LAN — use IP). Inventory: `home-nas/ansible/inventory.yml` → group `synapse_lxc`. Common debug commands:

```bash
ssh root@192.168.20.219 "docker ps"                                  # container status
ssh root@192.168.20.219 "tail -100 /data/sybra/home/logs/sybra.log"  # sybra-server logs
ssh root@192.168.20.219 "ls /data/sybra/home/tasks/"                 # task files
ssh root@192.168.20.219 "cat /data/sybra/home/config.yaml"           # server config
ssh root@192.168.20.219 "docker exec sybra sybra-cli list"           # CLI inside container
```

Bumping the deployed version = update image tag in `ansible/docker-compose/synapse-stack.yml`, run the deploy playbook.

**Toolchain inside the server container.** The image ships `mise` only — no Go, Node extras, or lint tools are pre-installed. Every project declares its own bootstrap, resolved from two layers per worktree:

1. `setup:` in the repo's `.sybra.yaml` — canonical toolchain, checked into git, identical on every machine.
2. `SetupCommands []string` in `~/.sybra/projects/<id>.yaml` — machine-local extras (optional), editable from the Project → Setup tab in the UI.

Commands from (1) run first, then (2). Sybra executes them in the worktree root with a 5-minute batch timeout; stdout/stderr stream to `~/.sybra/logs/worktrees/<task-id>-setup.log`. A non-zero exit aborts worktree creation (the agent never starts on a broken toolchain). The merge lives in `internal/project.MergeSetup` and is tested in `TestPrepareForTask_MergesRepoAndAppSetup`.

Typical repo `setup:` examples:

- Go/Node projects (e.g. synapse itself): `mise install` + `(cd frontend && npm ci)`
- pure npm: `npm ci`
- uv/Python: `uv sync`
- multi-step: `./.sybra/bootstrap.sh`

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
2. If exposing to frontend: add a method to a service struct in `internal/sybra/svc_*.go` (or to `App`).
3. Regenerate bindings: `wails3 generate bindings -ts -clean -d frontend/bindings ./...`.
4. Re-export from `frontend/src/lib/api.ts` so the rest of the frontend hits the shim.

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

**`mise run verify` is the pre-commit gate — it runs every deterministic, CI-aligned gate in `.github/workflows/ci.yml`** (frontend build:desktop + build:web, `go build ./...`, `go mod verify`, `go mod tidy` drift check, `go test -race ./...`, `go test -race -tags e2e ./internal/sybra/...`, golangci-lint, frontend check + test:coverage + oxlint + pin-strategy, api-shim sync, Wails bindings drift check, hadolint). "Deterministic" means the outcome depends only on repo state, not ambient CI infra — some steps (`npm ci`, Go module resolution) still need network access. It intentionally excludes the CI jobs that need external advisory DBs or a browser and so can't run as a reliable pre-commit loop — `lint-nilaway`, `security` (govulncheck + npm audit), and the Playwright `e2e` job; CI stays the source of truth for those three. Running only `go test ./...` skips the e2e suite entirely (it's gated behind `//go:build e2e`) and will ship green-local / red-CI.

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
- Reuses `internal/task.Store` + `internal/config.Load()` — same validation as GUI
- `mise run dev` auto-installs latest CLI before launching the desktop app
- `config dump` prints the resolved `~/.sybra/config.yaml` (env overrides applied, `todoist.api_token` redacted); `config doctor` sanity-checks data dirs, `agent.provider`, `agent.headless_permission_mode`, and enabled integrations missing required credentials. See `docs/CONFIG.md` (generated from `internal/config` struct tags via `go generate ./internal/config/...`) for the full key reference.

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
- ❌ Importing directly from `frontend/bindings/` in components/stores — go through `$lib/api` so the desktop ↔ web shim handles transport
- ❌ Using WebSocket/HTTP for Go↔Frontend IPC on desktop — Wails v3 events + bound services handle this
- ❌ Storing agent state in files — agents are in-memory only, tasks are file-backed
- ❌ Using `allowed_tools: []` without understanding the fallback is governed by `agent.require_permissions`/`agent.headless_permission_mode`, not always `--dangerously-skip-permissions` — see the permission-flag precedence under Agent Execution Modes
- ❌ Adding a new auto-task source without (a) an `Enabled bool` toggle in its config block and (b) `cfg.AllowsProjectType(...)` filtering if the source is project-scoped — both are required so users running Sybra on multiple machines can route work without duplication
- ❌ Adding a new pipeline status/stage without a matching handoff entry point — every stage a task can sit at must be directly reachable via `sybra-cli handoff --stage <name>`. That means: add the stage to `handoffStageTags` (`cmd/sybra-cli/main.go`) mapping it to a `handoff-<name>` tag, and add a `simple-task-handoff-<name>.yaml` builtin that flips a fresh task straight to that status while adopting its `worktree_dir`. Without this you cannot inject a task at the new stage to test/demo it in isolation (e.g. `--stage testing` → `simple-task-handoff-testing.yaml` → status `testing`)
- ❌ Baking project toolchains into the prod `Dockerfile` — the image ships `mise` only. Language-specific tools belong in each project's **Setup commands** (see Server Deployment section). New projects in new languages never require a container rebuild.
- ❌ Treating `go build .` (desktop) as a server-context commit gate — Wails v3 needs GTK/webkit on Linux (not installed server-side) and desktop is darwin-only/CI-owned. Use `mise run build:server` for server-side verification.
- ❌ Pasting, linking, or paraphrasing work-repo content (URLs, branches, SHAs, ticket IDs, snippets, logs, customer names) into sybra issues/PRs/tasks/commits — see **Work-Data Confidentiality** at the top. Any new auto-source that ingests external content must filter work-repo content at the source, not in post-processing.
- ❌ Surfacing `internal/artifact/` content in a GitHub issue/PR/comment without routing through `App.workScrubContextForTask` + `scrub.Scrub` first — the artifact store is raw/local-debug-only and never scrubs at write time.
