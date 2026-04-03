---
name: synapse-triage
description: Triage Synapse tasks — read incoming tasks, categorize, set priority tags, assign agent mode. Use when asked to triage, categorize, or prioritize tasks.
allowed-tools: Bash, Read, WebFetch
user-invocable: true
---

# Synapse Task Triage

Triage incoming tasks: analyze content, assign tags, set appropriate agent mode, update status.

## CLI Reference

The ONLY valid flags for `synapse-cli update` are: `--title`, `--status`, `--body`, `--mode`, `--tags`. Do NOT use `--agent-mode` or any other flag — they do not exist and will error.

## Process

### 1. List pending tasks

```bash
synapse-cli --json list --status new
```

### 2. For each task, analyze and categorize

Read the task body to understand scope:

```bash
synapse-cli --json get <id>
```

If the task title or body is just a URL with no description, fetch context and enrich the task:

```bash
# GitHub PR
gh pr view <url> --json title,body,files,additions,deletions

# GitHub issue
gh issue view <url> --json title,body,labels,comments

# Generic URL — use WebFetch to read page title/content
```

Update the task with a human-readable summary — replace the raw URL title with what it actually is, add key details to body, preserve original URL:

```bash
synapse-cli --json update <id> \
  --title "<concise title from fetched context>" \
  --body "Source: <url>
Files: N changed (+A/-D)

<description excerpt, max ~500 chars>"
```

### 3. Assign tags based on analysis

Common tag categories:
- **Domain**: `backend`, `frontend`, `infra`, `docs`, `ci`
- **Size**: `small`, `medium`, `large`
- **Type**: `bug`, `feature`, `refactor`, `review`

```bash
synapse-cli --json update <id> --tags "backend,small,review"
```

### 4. Set agent mode

- `headless` — automated tasks: code reviews, simple fixes, test writing
- `interactive` — tasks needing human guidance: architecture decisions, complex debugging

```bash
synapse-cli --json update <id> --mode headless
```

### 5. Update status when triaged

If the task is still `new` after assigning tags and mode, move it to `todo`:

```bash
synapse-cli --json update <id> --status todo
```

Skip this step if a previous update already changed the status.

## Decision Criteria

| Signal | Mode | Tags |
|--------|------|------|
| PR review URL | headless | review, size based on diff |
| Bug report with repro | headless | bug, domain from stack trace |
| Feature request, unclear scope | interactive | feature, large |
| Simple refactor/rename | headless | refactor, small |
| Architecture decision | interactive | feature, large |
