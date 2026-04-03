---
name: synapse-triage
description: Triage Synapse tasks — read incoming tasks, categorize, set priority tags, assign agent mode. Use when asked to triage, categorize, or prioritize tasks.
allowed-tools: Bash, Read, WebFetch
user-invocable: true
---

# Synapse Task Triage

Triage incoming tasks: analyze content, assign tags, set appropriate agent mode, update status.

## CLI Reference

The ONLY valid flags for `synapse-cli update` are: `--title`, `--status`, `--body`, `--mode`, `--tags`, `--project`. Do NOT use `--agent-mode` or any other flag — they do not exist and will error.

## Constraints

- Do NOT explore the codebase, read source files, or spawn sub-agents
- Triage based on title, body, and URL context only
- Keep total cost under $0.05 per task
- Code exploration happens during planning/implementation, not triage
- Ignore agent runs with a `role` field (triage, plan, eval, pr-fix) — those are system agents, not implementation agents

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

### 3. Add brief description if missing

If the task body is empty or has no meaningful context beyond a URL, add a 2-3 sentence description based on what you know from the title, URL context (if fetched), and general understanding. Do NOT explore the codebase or read source files — just clarify what the task is about and what "done" looks like.

```bash
synapse-cli --json update <id> \
  --body "Brief description of what needs to happen and expected outcome.

Original context preserved here if any."
```

Skip if the task already has a clear, descriptive body.

### 4. Assign tags based on analysis

Common tag categories:
- **Domain**: `backend`, `frontend`, `infra`, `docs`, `ci`
- **Size**: `small`, `medium`, `large`
- **Type**: `bug`, `feature`, `refactor`, `review`

```bash
synapse-cli --json update <id> --tags "backend,small,review"
```

### 5. Set agent mode

- `headless` — automated tasks: code reviews, simple fixes, test writing
- `interactive` — tasks needing human guidance: architecture decisions, complex debugging

```bash
synapse-cli --json update <id> --mode headless
```

### 6. Assign project (if applicable)

Check if the task references a known project (GitHub repo). List available projects:

```bash
synapse-cli --json project list
```

If the task body/URL matches a registered project, assign it:

```bash
synapse-cli --json update <id> --project "owner/repo"
```

### 7. Decide: planning or direct implementation

Complex tasks go to `planning` status (triggers auto-planning agent). Simple tasks go to `todo`.

```bash
# Complex tasks: medium/large features, architecture decisions → planning
synapse-cli --json update <id> --status planning

# Simple tasks: small bugs, refactors, reviews, chores → todo
synapse-cli --json update <id> --status todo
```

| Signal | Status |
|--------|--------|
| Size `medium` or `large` + type `feature` | planning |
| Architecture decision, unclear scope | planning |
| Size `small`, type `bug`/`refactor`/`review`/`chore` | todo |
| PR review | todo |

Step 7 already sets the status — no further status update needed. Skip if a previous step already changed the status.

## Decision Criteria

| Signal | Mode | Tags |
|--------|------|------|
| PR review URL | headless | review, size based on diff |
| Bug report with repro | headless | bug, domain from stack trace |
| Feature request, unclear scope | interactive | feature, large |
| Simple refactor/rename | headless | refactor, small |
| Architecture decision | interactive | feature, large |
