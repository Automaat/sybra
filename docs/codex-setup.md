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
  model: gpt-5.4   # optional; gpt-5.4 is the default
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
| `sonnet` (default) | `gpt-5.4` |
| `opus` | `gpt-5.4` |
| `haiku` | `gpt-5.4-mini` |
| any other string | passed through verbatim |

## How Codex Runs in Sybra

### Headless mode

Sybra spawns a `codex exec` subprocess per task:

```bash
# Default (no permission restrictions)
codex exec --json --skip-git-repo-check --full-auto --model gpt-5.4 -C <worktree> "<prompt>"

# With RequirePermissions=true
codex exec --json --skip-git-repo-check --sandbox workspace-write --model gpt-5.4 -C <worktree> "<prompt>"
```

Stdout is read as NDJSON. Sybra parses Codex event types (`agent_message`, `command_execution`, `task_complete`, etc.) and maps them to its unified `StreamEvent` format for display.

### Interactive (conversational) mode

Sybra spawns a new `codex exec --json` process for each user turn. Unlike Claude conversational mode (which keeps a single process alive on stdin), each Codex turn is a discrete subprocess invocation. This means there is **no persistent stdin pipe** — the UI sends messages by launching a new process with the follow-up prompt.

## Differences vs Claude Code

| Feature | Claude Code | Codex |
|---------|-------------|-------|
| Session resume | `--resume <session-id>` | Not supported — each run is independent |
| Tool allowlist | `--allowedTools tool1,tool2` | Not supported; use `--sandbox workspace-write` for file-write-only mode |
| Permissions bypass | `--dangerously-skip-permissions` | `--full-auto` |
| Session files | `~/.claude/projects/<key>/<id>.jsonl` | `~/.codex/sessions/rollout-<id>.jsonl` |
| External discovery | Claude process detection via session files | Codex process detection via `pgrep -f codex` + session JSONL |
| Cost reporting | Reported in `result` event (`cost_usd`) | Not reported in stream; billed on OpenAI dashboard |
| Conversational model | Single long-lived process with stdin pipe | New subprocess per turn |

## Commit Requirements

Codex agents must commit their work before finishing — the same requirement as Claude agents. Git commit flags (`-s` for sign-off, `-S` for GPG signing) work normally inside Sybra-managed worktrees. See the orchestrator's "Agent Commit Requirement" section for the required commit block to include in every headless prompt.

## Skill and Prompt Compatibility

Existing Sybra skills (`/sybra-tasks`, `/sybra-triage`, `/sybra-plan`, etc.) are provider-agnostic — they use `sybra-cli` and shell commands, not the Claude SDK directly. They work without modification under either provider.

The orchestrator prompt (`orchestrator/CLAUDE.md`) governs the Sybra orchestrator session, which always runs as a Claude Code agent. Codex is used for implementation agents dispatched by the orchestrator, not for the orchestrator itself.

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
