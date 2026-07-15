---
name: sybra-tasks
description: Interactive operator task CRUD for Sybra when a human explicitly asks to list, create, update, or delete tasks in chat. Do not use for workflow-dispatched plan/review/test/implementation agents or prompts that already specify task-stage instructions.
allowed-tools: Bash
user-invocable: true
disable-model-invocation: true
argument-hint: "[list|create|update|delete] [args]"
---

# Sybra Task Management

Use `sybra-cli` to manage tasks. Always use `--json` for machine-parseable output.

Only run when a human/operator explicitly asks to manage Sybra tasks. Never activate inside workflow-dispatched agents or merely because a prompt mentions tasks, status fields, tags, or stage transitions.

## Commands

```bash
# List all tasks
sybra-cli --json list

# Filter by status (todo, in-progress, in-review, done)
sybra-cli --json list --status todo

# Filter by tag
sybra-cli --json list --tag backend

# Get single task
sybra-cli --json get <id>

# Create task
sybra-cli --json create --title "task title" --body "markdown body" --mode headless --tags "tag1,tag2"

# Update task fields (only specify what changes)
sybra-cli --json update <id> --status in-progress
sybra-cli --json update <id> --title "new title" --tags "new,tags"

# Delete task
sybra-cli --json delete <id>
```

## Task Fields

- **status**: `new` | `todo` | `in-progress` | `in-review` | `human-required` | `done`
- **mode**: `headless` (automated claude -p) | `interactive` (tmux session)
- **tags**: comma-separated labels

## Workflow

1. `sybra-cli --json list --status todo` — see open tasks
2. `sybra-cli --json update <id> --status in-progress` — pick up task
3. Do the work
4. `sybra-cli --json update <id> --status done` — mark complete

## Output Format

Single task: JSON object with `id`, `title`, `status`, `agentMode`, `tags`, `body`, `createdAt`, `updatedAt`.
List: JSON array of task objects. Empty list returns `[]`.
Delete: `{"deleted": "<id>"}`.
Errors: stderr `{"error": "message"}` with non-zero exit.

<example>
Input: User says "show me all open backend tasks".

Command: `sybra-cli --json list --status todo --tag backend`

Output: JSON array of tasks matching both filters.
</example>

<example>
Input: User says "create a task to fix the login bug, tag as auth, headless mode".

Command:
```bash
sybra-cli --json create --title "fix login bug" --mode headless --tags "auth,bug"
```

Output: `{"id": "task-xyz", "title": "fix login bug", "status": "new", ...}`
</example>

<example>
Input: User says "mark task-abc as in progress".

Command: `sybra-cli --json update task-abc --status in-progress`

Output: Updated task JSON.
</example>
