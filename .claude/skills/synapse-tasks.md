---
name: synapse-tasks
description: Manage Synapse tasks (list, create, update, delete) via synapse-cli. Use when user mentions tasks, work items, TODOs, or asks to track/create/update work.
allowed-tools: Bash
user-invocable: true
argument-hint: "[list|create|update|delete] [args]"
---

# Synapse Task Management

Use `synapse-cli` to manage tasks. Always use `--json` for machine-parseable output.

## Commands

```bash
# List all tasks
synapse-cli --json list

# Filter by status (todo, in-progress, in-review, done)
synapse-cli --json list --status todo

# Filter by tag
synapse-cli --json list --tag backend

# Get single task
synapse-cli --json get <id>

# Create task
synapse-cli --json create --title "task title" --body "markdown body" --mode headless --tags "tag1,tag2"

# Update task fields (only specify what changes)
synapse-cli --json update <id> --status in-progress
synapse-cli --json update <id> --title "new title" --tags "new,tags"

# Delete task
synapse-cli --json delete <id>
```

## Task Fields

- **status**: `new` | `todo` | `in-progress` | `in-review` | `human-required` | `done`
- **mode**: `headless` (automated claude -p) | `interactive` (tmux session)
- **tags**: comma-separated labels

## Workflow

1. `synapse-cli --json list --status todo` — see open tasks
2. `synapse-cli --json update <id> --status in-progress` — pick up task
3. Do the work
4. `synapse-cli --json update <id> --status done` — mark complete

## Output Format

Single task: JSON object with `id`, `title`, `status`, `agentMode`, `tags`, `body`, `createdAt`, `updatedAt`.
List: JSON array of task objects. Empty list returns `[]`.
Delete: `{"deleted": "<id>"}`.
Errors: stderr `{"error": "message"}` with non-zero exit.

<example>
Input: User says "show me all open backend tasks".

Command: `synapse-cli --json list --status todo --tag backend`

Output: JSON array of tasks matching both filters.
</example>

<example>
Input: User says "create a task to fix the login bug, tag as auth, headless mode".

Command:
```bash
synapse-cli --json create --title "fix login bug" --mode headless --tags "auth,bug"
```

Output: `{"id": "task-xyz", "title": "fix login bug", "status": "new", ...}`
</example>

<example>
Input: User says "mark task-abc as in progress".

Command: `synapse-cli --json update task-abc --status in-progress`

Output: Updated task JSON.
</example>
