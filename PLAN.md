# Synapse — Local Agent Orchestrator

## Context

Build a local desktop app to orchestrate a swarm of Claude Code agents. Markdown-based task management so agents can natively read/write tasks. Two execution modes: interactive (tmux) and headless (`claude -p`). Wails v2 GUI for high-perf local experience.

## Tech Stack

- **Backend**: Go (Wails v2 bound methods)
- **Frontend**: Svelte + TypeScript (Vite, auto-generated bindings)
- **UI**: Skeleton UI (skeleton.dev) + Vox theming
- **IPC**: Wails built-in events (no WebSocket needed)
- **File watching**: fsnotify
- **Agent control**: `os/exec` → tmux CLI + `claude` CLI
- **Task format**: YAML frontmatter + GFM markdown

## Project Structure

```
synapse/
├── main.go                        # Wails bootstrap
├── app.go                         # Bound methods exposed to frontend
├── wails.json
├── go.mod
├── internal/
│   ├── task/
│   │   ├── model.go               # Task struct, statuses
│   │   ├── parser.go              # YAML frontmatter + markdown parser
│   │   └── store.go               # CRUD against tasks/ directory
│   ├── agent/
│   │   ├── model.go               # Agent struct, states, StreamEvent
│   │   ├── manager.go             # Lifecycle: start/stop/pause/resume/list
│   │   └── runner_headless.go     # claude -p NDJSON stream parser
│   ├── tmux/
│   │   └── manager.go             # tmux session CRUD via os/exec
│   ├── watcher/
│   │   └── watcher.go             # fsnotify on tasks/ dir, debounced
│   └── github/
│       └── interface.go           # Future: interface only
├── tasks/                         # Markdown task files
├── frontend/
│   ├── src/
│   │   ├── App.svelte             # Router setup
│   │   ├── main.ts                # Entry point
│   │   ├── pages/
│   │   │   ├── Dashboard.svelte   # Agent grid + stats
│   │   │   ├── TaskList.svelte    # Filterable task list
│   │   │   ├── TaskDetail.svelte  # Single task view + actions
│   │   │   └── AgentDetail.svelte # Agent output + controls
│   │   ├── components/
│   │   │   ├── AgentCard.svelte
│   │   │   ├── TaskCard.svelte
│   │   │   ├── StreamOutput.svelte # Headless NDJSON log viewer
│   │   │   ├── TerminalView.svelte # tmux capture-pane viewer
│   │   │   └── StatusBadge.svelte
│   │   ├── stores/
│   │   │   ├── tasks.ts           # Svelte store for tasks
│   │   │   └── agents.ts          # Svelte store for agents
│   │   └── types/
│   │       └── models.ts
│   └── wailsjs/                   # Auto-generated
```

## Core Models

### Task (YAML frontmatter .md file)

```yaml
---
id: task-abc123
title: Implement auth middleware
status: todo              # todo|in-progress|done|blocked
agent_mode: headless      # interactive|headless
allowed_tools: []         # empty = all tools allowed
tags: [backend, auth]
created_at: 2026-04-02T10:00:00Z
updated_at: 2026-04-02T10:00:00Z
---
## Description
Add JWT middleware to the API router.

## Checklist
- [ ] Create middleware function
- [ ] Write tests
```

### Agent (in-memory Go struct)

Fields: ID, TaskID, Mode (interactive|headless), State (idle|running|paused|stopped), SessionID (claude session for --resume), TmuxSession name, CostUSD, StartedAt, OutputBuffer ([]StreamEvent), cmd, cancel func

### StreamEvent (NDJSON from claude -p)

Types: init, assistant, tool_use, tool_result, result — each with type, content, session_id, cost_usd fields

## Agent Execution Modes

### Headless (`claude -p`)
```bash
claude -p "prompt" --output-format stream-json [--resume <id>] [--allowedTools "..."]
```
- Go spawns process, reads stdout line-by-line, unmarshals NDJSON
- Each event emitted to frontend via `runtime.EventsEmit(ctx, "agent:output:<id>", event)`
- On `result` event: extract session_id + cost, update agent state
- Permissions: per-task `allowed_tools` field in frontmatter → `--allowedTools` flag. Empty = all tools with `--dangerously-skip-permissions`

### Interactive (tmux)
```bash
tmux new-session -d -s synapse-<id> -x 200 -y 50 "claude"
```
- User can attach: `tmux attach -t synapse-<id>`
- GUI polls `tmux capture-pane -t synapse-<id> -p` every 1s for preview
- GUI provides "Attach in Terminal" button

## Key Backend Methods (bound to frontend)

```go
// app.go — all exposed to Svelte via auto-generated bindings
func (a *App) ListTasks() []task.Task
func (a *App) GetTask(id string) task.Task
func (a *App) CreateTask(title, body, mode string) task.Task
func (a *App) UpdateTask(id string, updates map[string]interface{}) task.Task

func (a *App) StartAgent(taskID, mode, prompt string) agent.Agent
func (a *App) StopAgent(agentID string) error
func (a *App) PauseAgent(agentID string) error
func (a *App) ResumeAgent(agentID string) error
func (a *App) ListAgents() []agent.Agent
func (a *App) GetAgentOutput(agentID string) []agent.StreamEvent
```

## Wails Events (Go → Frontend)

- `agent:state:<id>` — agent state change (running/stopped/etc)
- `agent:output:<id>` — new StreamEvent from headless agent
- `task:updated` — task markdown file changed (via fsnotify)
- `task:created` / `task:deleted` — file created/removed

## Implementation Phases

### Phase 1 — Scaffold [DONE]
- [x] `wails init -n synapse -t svelte-ts`
- [x] Svelte 5 + Tailwind 4 + Skeleton v4 + Vox theme
- [x] Create Go package dirs, define all model structs
- [x] Wire app.go with bound methods
- [x] golangci-lint v2 strict config (gocritic, nilerr, nilnesserr, nilnil, nolintlint, modernize)
- [x] oxlint for frontend linting
- [x] GitHub Actions CI (lint-go, lint-frontend, build)
- [x] mise.toml (Go 1.26.1, Node 22, dev/build/lint tasks)

### Phase 2 — Task System [DONE]
- [x] `task/parser.go`: split frontmatter + body, marshal/unmarshal
- [x] `task/store.go`: List/Get/Create/Update against tasks/ dir
- [x] Frontend: TaskList + TaskDetail pages with Skeleton components
- [x] Svelte stores for reactive task state
- [x] Create sample .md files
- [x] Playwright e2e tests (14 tests against wails dev on :34115)
- [x] GitHub Actions e2e job (macos-15, wails dev + Playwright)

### Phase 3 — Headless Agent
- `agent/runner_headless.go`: spawn claude -p, parse NDJSON stream
- `agent/manager.go`: Start/Stop for headless mode
- Per-task allowed_tools → --allowedTools flag
- Frontend: AgentDetail + StreamOutput component
- Wire Wails events for real-time output

### Phase 4 — tmux Interactive
- `tmux/manager.go`: Create/SendKeys/CapturePaneOutput/Kill/Exists
- Add interactive mode to agent manager
- Frontend: TerminalView with capture-pane polling + attach button

### Phase 5 — File Watcher
- `watcher/watcher.go`: fsnotify on tasks/, 200ms debounce
- Emit task:updated events, frontend auto-refreshes

### Phase 6 — Dashboard + Polish
- Dashboard page: running agents grid, task status summary, cost tracking
- Agent pause/resume, graceful shutdown (kill tmux sessions on exit)
- Error handling, edge cases

### Phase 7 — GitHub (future)
- `github/interface.go` interface definition
- Concrete client using go-github
- Sync tasks ↔ GitHub issues

## Verification

1. `wails dev` — app launches, hot reload works
2. Create task via GUI → .md file appears in tasks/
3. Start headless agent → stream output appears in real-time
4. Start interactive agent → tmux session created, can attach
5. Edit .md file externally → GUI updates automatically
6. Stop agent → process killed, state updated
