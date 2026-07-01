# Codex Agent Setup

Sybra supports [OpenAI Codex CLI](https://github.com/openai/codex) as an alternative agent provider alongside Claude Code. This document covers local setup, authentication, Sybra configuration, and known behavioral differences.

## Prerequisites

### Install Codex CLI

```bash
npm install -g @openai/codex
```

Verify the binary is on `PATH`:

```bash
codex --version
```

### Authenticate

Codex requires an OpenAI API key. Set it in your shell environment before starting Sybra:

```bash
export OPENAI_API_KEY=sk-...
```

Add the export to your shell profile (`~/.zshrc`, `~/.bashrc`) so it persists across sessions. Sybra inherits the parent process environment — the key must be set before the app launches.

If you prefer interactive login:

```bash
codex auth
```

> **macOS GUI launch note:** Apps launched from Finder or Spotlight do not inherit shell profile exports. Set `OPENAI_API_KEY` via `launchctl setenv` or a `launchd` plist to ensure Sybra picks it up:
>
> ```bash
> launchctl setenv OPENAI_API_KEY "sk-..."
> # then relaunch Sybra
> ```

## Enable Codex in Sybra

Edit `~/.sybra/config.yaml`:

```yaml
agent:
  provider: codex
  model: gpt-5.5   # optional; gpt-5.5 is the default
```

To revert to Claude:

```yaml
agent:
  provider: claude
```

### Model Aliases

Sybra maps generic aliases to provider-specific model IDs at runtime:

| Sybra alias | Codex model |
|---------------|-------------|
| `sonnet` (default) | `gpt-5.5` |
| `opus` | `gpt-5.5` |
| `haiku` | `gpt-5.4-mini` |
| any other string | passed through verbatim |

## How Codex Runs in Sybra

### Headless mode

Sybra spawns a `codex exec` subprocess per task:

```bash
# Headless mode always uses bypass (regardless of RequirePermissions)
codex exec --json --skip-git-repo-check --ignore-user-config --ignore-rules --dangerously-bypass-approvals-and-sandbox --model gpt-5.5 -C <worktree> "<prompt>"
```

`--sandbox workspace-write` is intentionally not used in headless mode. That mode requests user approval for writes outside the workspace; in a headless run there is no TTY or UI to serve the approval prompt, so every such request is auto-rejected and the agent run fails. The worktree directory itself provides task isolation.

`RequirePermissions=true` only affects sandbox selection in **interactive (conversational)** mode, where a human can approve sandbox prompts.

Stdout is read as NDJSON. Sybra parses Codex event types (`agent_message`, `command_execution`, `task_complete`, etc.) and maps them to its unified `StreamEvent` format for display.

### Interactive (conversational) mode

Sybra spawns a new `codex exec --json` process for each user turn. Unlike Claude conversational mode (which keeps a single process alive on stdin), each Codex turn is a discrete subprocess invocation. This means there is **no persistent stdin pipe** — the UI sends messages by launching a new process with the follow-up prompt.

## Differences vs Claude Code

| Feature | Claude Code | Codex |
|---------|-------------|-------|
| Session resume | `--resume <session-id>` | Not supported — each run is independent |
| Tool allowlist | `--allowedTools tool1,tool2` | Not supported; use `--sandbox workspace-write` for file-write-only mode |
| Permissions bypass | `--dangerously-skip-permissions` | `--dangerously-bypass-approvals-and-sandbox` |
| Session files | `~/.claude/projects/<key>/<id>.jsonl` | `~/.codex/sessions/rollout-<id>.jsonl` |
| External discovery | Claude process detection via session files | Codex process detection via `pgrep -f codex` + session JSONL |
| Cost reporting | Reported in `result` event (`cost_usd`) | Not reported in stream; billed on OpenAI dashboard |
| Conversational model | Single long-lived process with stdin pipe | New subprocess per turn |

## Commit Requirements

Codex agents must commit their work before finishing — the same requirement as Claude agents. Git commit flags (`-s` for sign-off, `-S` for GPG signing) work normally inside Sybra-managed worktrees. See the orchestrator's "Agent Commit Requirement" section for the required commit block to include in every headless prompt.

## Skill and Prompt Compatibility

Existing Sybra skills (`/sybra-tasks`, `/sybra-triage`, `/sybra-plan`, etc.) are provider-agnostic — they use `sybra-cli` and shell commands, not the Claude SDK directly. They work without modification under either provider.

The orchestrator prompt (`orchestrator/CLAUDE.md`) governs the Sybra orchestrator session, which always runs as a Claude Code agent. Codex is used for implementation agents dispatched by the orchestrator, not for the orchestrator itself.

## Hook Telemetry And Validation

Sybra injects hooks into every `codex exec` invocation using inline `-c hooks.<Event>=...` config overrides. This is the only channel that survives Sybra's `--ignore-user-config --ignore-rules` flags — per-worktree `.codex/hooks.json` and `~/.codex` plugin files are explicitly ignored by those flags and do not fire.

### Instrumented events

| Event | Purpose |
|-------|---------|
| `PreToolUse` | Klaudiush validation before tool execution |
| `SessionStart` | Observe-only `codex.session.start` audit event |
| `SubagentStart` | Observe-only `codex.subagent.start` audit event |
| `SubagentStop` | Observe-only `codex.subagent.stop` audit event |
| `Stop` | Observe-only `codex.session.stop` audit event |

`PreToolUse` uses Codex's foreground command-hook default so klaudiush can deny unsafe git commands such as commits missing required `-s`/`-S` flags. Lifecycle hooks run in the background and remain fail-open telemetry.

### How it works

For each event, Sybra appends a `-c` override to the `codex exec` command:

```
-c 'hooks.PreToolUse=[{hooks=[{type="command",command="klaudiush --provider codex --event PreToolUse",timeout_seconds=30}]}]'
-c 'hooks.SessionStart=[{hooks=[{type="command",command="sybra-cli hook SessionStart --task <id>",run_mode="background",timeout_seconds=5}]}]'
```

The `--dangerously-bypass-hook-trust` flag is also passed so codex executes the hook without requiring a trusted source check.

`klaudiush` reads the provider hook payload from stdin and returns the provider-native permission decision JSON. Sybra omits the klaudiush hook only when the binary is not resolvable on `PATH` or adjacent to the Sybra binary.

`sybra-cli hook <Event> --task <id>` is the lifecycle telemetry receiver: it reads the JSON payload from stdin, maps it to a structural-only `audit.Event` (session_id, subagent_id, kind, model — never cwd, prompts, tool_input, or file paths), and appends it to the daily audit NDJSON (`~/.sybra/logs/audit/<date>.ndjson`). The receiver always exits 0 (fail-open) so hook errors never stall the agent run.

### Fail-open behaviour

Hooks are omitted (the codex run proceeds normally, without the affected hook) when:
- `klaudiush` or `sybra-cli` is not on `PATH` and not adjacent to the Sybra binary
- The resolved hook binary path contains shell-sensitive characters
- The task ID is empty or contains characters outside `[a-zA-Z0-9._/-]`

### Minimum supported codex version

`--dangerously-bypass-hook-trust` is required and was verified present in codex **0.142.2**. Hooks fire via `-c` config overrides starting from codex **0.131** (when plugin hooks became default-enabled). Sybra does not probe the codex version at runtime — if the flag is unsupported, codex will reject the invocation and the agent will fail to start; update to 0.142.2+ in that case.

### Viewing hook events

```bash
sybra-cli audit --type codex.session.start
sybra-cli audit --type codex.subagent.start
sybra-cli audit --since 1h --summary
```

## Troubleshooting

**`codex: command not found`**

```bash
npm install -g @openai/codex
# Ensure npm global bin is on PATH:
export PATH="$(npm prefix -g)/bin:$PATH"
```

**Authentication errors at runtime**

```bash
echo $OPENAI_API_KEY   # must be non-empty
codex auth             # re-authenticate interactively
```

**Agent starts but produces no events**

Confirm `OPENAI_API_KEY` is set in Sybra's process environment (see macOS GUI note above). Also verify the `codex` binary path is in the `PATH` that Sybra sees:

```bash
# Check from Sybra's inherited PATH
which codex
```

**External Codex sessions not appearing**

Sybra discovers live Codex sessions via `pgrep -f codex` and reads JSONL from `~/.codex/sessions/`. Sessions appear as `ext-codex-*` agents in the UI. If sessions are absent, confirm:

1. The Codex process is running (`pgrep -f codex`)
2. Session files exist at `~/.codex/sessions/rollout-<id>.jsonl`
3. The process has had time to write at least one event to the JSONL file
