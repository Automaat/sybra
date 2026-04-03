# Synapse

Local desktop app to orchestrate a swarm of Claude Code agents. Markdown-based task management, two execution modes (interactive tmux + headless `claude -p`), Wails v2 GUI.

## Project Structure

```
synapse/
├── main.go                  # Wails bootstrap, embeds frontend/dist
├── app.go                   # Bound methods exposed to Svelte frontend
├── wails.json               # Wails config
├── go.mod / go.sum
├── internal/
│   ├── task/                # YAML frontmatter + markdown task CRUD
│   │   ├── model.go         # Task struct, Status enum
│   │   ├── parser.go        # Frontmatter parse/marshal
│   │   └── store.go         # Filesystem-backed store
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
│   └── synapse-cli/         # CLI for task CRUD (used by Claude Code skills)
│       └── main.go
├── .claude/
│   └── skills/              # Claude Code skills (auto-copied to ~/.synapse/skills on start)
│       ├── synapse-tasks.md # Task CRUD skill
│       └── synapse-triage.md # Triage workflow skill
├── tasks/                   # Markdown task files (runtime data)
├── frontend/
│   ├── src/
│   │   ├── App.svelte       # Root component
│   │   ├── main.ts          # Entry point
│   │   └── style.css
│   ├── wailsjs/             # Auto-generated Wails bindings
│   └── package.json
└── build/                   # Wails build assets
```

## Tech Stack

### Backend

- **Go 1.26.1** (Wails v2 bound methods)
- **Wails v2.12** — desktop app framework, IPC via bound methods + events
- **fsnotify** — file watching for task changes
- **gopkg.in/yaml.v3** — YAML frontmatter parsing

### Frontend

- **Svelte 5** + **TypeScript 6** (Vite 8)
- **Skeleton UI v4** (skeleton.dev) + Vox theme
- **Tailwind CSS v4**
- Auto-generated Wails bindings in `frontend/wailsjs/`

### Tooling

- **mise** — tool version management (Go 1.26.1, Node 24)
- **golangci-lint v2** — Go linting (gocritic, nilerr, nilnesserr, nilnil, nolintlint, modernize)
- **oxlint** — frontend linting
- **GitHub Actions** — CI (lint-go, lint-frontend, build)

## Architecture

### Wails Binding Convention

All methods on `App` struct in `app.go` are auto-bound to the frontend. Wails generates TypeScript bindings in `frontend/wailsjs/`.

**Adding a new bound method:**
1. Add method to `App` struct in `app.go`
2. Run `wails dev` or `wails generate module` to regenerate bindings
3. Import from `wailsjs/go/main/App` in Svelte

**Wails events (Go → Frontend):**
- `agent:state:<id>` — agent state change
- `agent:output:<id>` — new StreamEvent from headless agent
- `task:updated` / `task:created` / `task:deleted` — file system changes

Emit events via `runtime.EventsEmit(ctx, "event:name", data)`.

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

**Storage:** `~/.synapse/projects/` (YAML metadata), `~/.synapse/clones/` (bare git repos), `~/.synapse/worktrees/` (per-task checkouts).

**Flow:** Create project from URL → bare clone → assign `project_id` to tasks → agent start auto-creates worktree → worktree cleaned up on agent completion.

**CLI:**
```bash
synapse-cli project list|get|create|delete
synapse-cli create --title "..." --project "owner/repo"
```

### Agent Execution Modes

**Headless** (`claude -p`):
```bash
claude -p "prompt" --output-format stream-json [--resume <id>] [--allowedTools "..."]
```
- Go spawns process, reads stdout NDJSON line-by-line
- StreamEvent types: `init`, `assistant`, `tool_use`, `tool_result`, `result`
- Empty `allowed_tools` → `--dangerously-skip-permissions`

**Interactive** (tmux):
```bash
tmux new-session -d -s synapse-<id> -x 200 -y 50 "claude"
```
- GUI polls `tmux capture-pane -t synapse-<id> -p` for preview
- User attaches via terminal

## Development Workflow

### Running Locally

```bash
mise run dev          # wails dev — hot reload for both Go + Svelte
```

### Adding a Backend Feature

1. Add/modify Go types in `internal/<package>/`
2. If exposing to frontend: add bound method to `app.go`
3. Run `wails dev` to regenerate frontend bindings
4. Use new binding in Svelte via `import { MethodName } from 'wailsjs/go/main/App'`

### Adding a Frontend Feature

1. Create/edit Svelte component in `frontend/src/`
2. Use Skeleton UI components from `@skeletonlabs/skeleton-svelte`
3. Call Go backend via auto-generated bindings in `wailsjs/`
4. Listen for events with `runtime.EventsOn("event:name", callback)`

### Testing

- Go: `go test ./...`
- Use table-driven tests for Go packages
- Frontend: `cd frontend && npm run check` (svelte-check)

## Quality Gates

Before committing:

- [ ] golangci-lint passes
- [ ] oxlint passes
- [ ] svelte-check passes
- [ ] Go tests pass
- [ ] `wails build` succeeds

```bash
# Lint all
mise run lint

# Go tests
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

## CLI (`synapse-cli`)

Standalone binary for task CRUD, used by Claude Code skills. Installed via `go install ./cmd/synapse-cli`.

```bash
synapse-cli [--json] <command> [flags]

list     [--status STATUS] [--tag TAG]
get      <id>
create   --title TITLE [--body BODY] [--mode MODE] [--tags t1,t2]
update   <id> [--title T] [--status S] [--body B] [--mode M] [--tags T]
delete   <id>
```

- `--json` for machine-parseable output (used by skills)
- Reuses `internal/task.Store` + `internal/config.Load()` — same validation as GUI
- `mise run dev` auto-installs latest CLI before starting wails

### Skills

Project-local Claude Code skills in `.claude/skills/`:
- `synapse-tasks.md` — task CRUD via CLI (`/synapse-tasks`)
- `synapse-triage.md` — triage workflow (`/synapse-triage`)

Skills are auto-copied to `~/.synapse/skills/` on app startup (via `syncSkills()` in `app.go`).

### Orchestrator Brain

`orchestrator/CLAUDE.md` — system instructions for Claude Code orchestrator sessions. Copied to `~/.synapse/CLAUDE.md` on app start. Covers: triage rules, dispatch logic, monitoring, failure handling, escalation criteria.

## Build Order

Frontend must build before Go compilation due to `//go:embed all:frontend/dist`:

1. `cd frontend && npm install && npm run build` → produces `frontend/dist/`
2. `wails build` (or `go build`) — embeds `frontend/dist/` into binary

`wails dev` and `wails build` handle this automatically. Manual `go build` requires step 1 first.

## Anti-Patterns

**AVOID:**

- ❌ Running `go build` without building frontend first — `//go:embed` fails if `frontend/dist/` missing
- ❌ Forgetting to regenerate Wails bindings after changing `app.go` methods
- ❌ Using WebSocket/HTTP for Go↔Frontend IPC — Wails events + bound methods handle this
- ❌ Storing agent state in files — agents are in-memory only, tasks are file-backed
- ❌ Editing files in `frontend/wailsjs/` — these are auto-generated, changes get overwritten
- ❌ Using `allowed_tools: []` without understanding it means all tools with `--dangerously-skip-permissions`
