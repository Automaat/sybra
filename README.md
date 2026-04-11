# Synapse

Autonomous Claude Code orchestrator. Local desktop app that manages a swarm of AI agents: triage incoming work, spawn agents, monitor progress, handle failures — all through a markdown-based task board.

## What it does

You create tasks. Synapse triages them, spawns Claude Code agents to implement them, monitors progress, reviews results, and keeps the board healthy. Each agent gets an isolated git worktree so parallel work never steps on itself.

```
new → todo → in-progress → in-review → done
              ↑
         planning → plan-review → [human approves] → todo
```

Complex tasks go through a planning phase. Simple tasks go straight to execution.

## Features

- **Task board** — drag-and-drop Kanban with status swimlanes, priority, tags, agent mode
- **Dual execution modes** — headless (`claude -p` with NDJSON streaming) or interactive (tmux sessions)
- **Worktree isolation** — each agent gets a per-task git worktree from a bare clone; no conflicts between concurrent agents
- **GitHub integration** — link tasks to repos; agents clone, commit, and open PRs automatically
- **Eval agents** — post-implementation verification: confirm commits exist, PRs are open, quality gates pass
- **Chat UI** — VS Code-like conversation view per agent with real-time output streaming
- **Planning workflow** — complex tasks get a plan that humans review before implementation starts
- **Spotlight** — keyboard-driven task creation from anywhere in the app
- **Todoist sync** — bidirectional sync with configurable polling
- **Audit log** — structured NDJSON event log for failure analysis and cycle-time tracking
- **CLI** (`synapse-cli`) — task CRUD from the terminal, used by Claude Code skills

## Tech stack

| Layer | Stack |
|-------|-------|
| Backend | Go 1.26.2, Wails v2.12 |
| Frontend | Svelte 5, TypeScript 6, Skeleton UI v4, Tailwind v4 |
| IPC | Wails bound methods + events (no HTTP, no WebSocket) |
| File format | YAML frontmatter + GFM markdown |
| Tooling | mise, golangci-lint v2, oxlint, GitHub Actions |

## Getting started

**Prerequisites:** Go 1.26.2, Node 24, [mise](https://mise.jdx.dev/), [Wails v2](https://wails.io/)

```bash
# Install tool versions
mise install

# Dev server with hot reload (Go + Svelte)
mise run dev

# Production build
mise run build

# Install CLI
go install ./cmd/synapse-cli
```

## CLI

```bash
synapse-cli [--json] <command> [flags]

list     [--status STATUS] [--tag TAG] [--project PROJECT]
get      <id>
create   --title TITLE [--body BODY] [--mode MODE] [--tags t1,t2]
update   <id> [--title T] [--status S] [--body B] [--mode M] [--tags T]
delete   <id>
project  list|get|create|delete
audit    [--since 7d] [--summary]
```

Use `--json` for machine-readable output (required by Claude Code skills).

## Task format

```yaml
---
id: task-abc123
title: Implement auth middleware
status: todo
agent_mode: headless      # headless | interactive
tags: [backend, auth, medium]
project_id: owner/repo
---
## Description
What needs to be done.
```

Tasks live in `~/.synapse/tasks/`. The app watches for file changes and updates the board in real time.

## Projects

Projects mirror GitHub repos as bare clones. Assign a `project_id` to a task and Synapse automatically creates an isolated worktree when the agent starts.

```bash
synapse-cli project create --url https://github.com/owner/repo
synapse-cli create --title "Add feature" --project "owner/repo"
```

## Orchestrator

Synapse ships with a Claude Code system prompt (`orchestrator/CLAUDE.md`) that turns a Claude Code session into the orchestration brain — triage, dispatch, monitor, resolve. Load it in any Claude Code session pointed at `~/.synapse/`.

Claude Code skills (`synapse-tasks`, `synapse-triage`, `synapse-plan`, `synapse-evaluate`) are auto-installed to `~/.synapse/skills/` on app startup.

## File naming conventions

Files at the repo root follow two prefixes:

- **`svc_*.go`** — stateless service handlers. Each file declares a `*Service` struct that takes one or more internal managers as dependencies and exposes domain operations. No Wails coupling; these can be unit-tested without a running app.
- **`app_*.go`** — lifecycle methods and adapters on `*App`. These files bind Wails IPC, handle event emission, run background loops, and wire `*Service` objects into the desktop runtime. Methods here call into `svc_*` types rather than duplicating logic.

## Quality gates

```bash
mise run lint      # golangci-lint + oxlint
go test ./...
cd frontend && npm run check   # svelte-check
mise run build     # confirms embed + compilation
```
