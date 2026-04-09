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
- Always shorten titles >80 chars — keep them actionable/descriptive, not just truncated
- When shortening, move the verbose original title into the task body (prepend, preserve existing content)

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

### 3. Detect and rewrite freeform instruction-style titles

Check if the title looks like natural-language/casual instruction rather than a structured task title. Indicators:

- Starts with "i ", "we ", "maybe ", "i think", "i want", "could we", "let's", "should we", etc.
- Written as a sentence/thought, not an imperative action
- Contains filler phrases like "i often", "i feel like", "sometimes", "random stuff"
- Clearly conversational in tone

If detected, rewrite the title as a concise, imperative, structured task title. Move the original freeform text into the body so context is preserved.

Examples:
- "in synapse i often write random stuff as task name" → "feat(triage): rewrite freeform titles into structured task titles"
- "i think we should add auth" → "feat(auth): add authentication"
- "make the button blue" → "fix(ui): change button color to blue"
- "we should probably fix the login bug" → "fix(auth): resolve login failure"

Use conventional commit format (`type(scope): description`) when the domain/type is clear. Otherwise use a plain imperative title.

```bash
synapse-cli --json update <id> \
  --title "<structured imperative title>" \
  --body "**Original instruction:** <original freeform title>

<existing body content preserved here>"
```

Skip if the title is already structured/imperative.

### 4. Shorten title if too long

If the title exceeds 80 characters, rewrite it as a concise ≤80 char summary. Move the original verbose title into the task body so no context is lost.

Read the current body first (already done in step 2), then update with both `--title` and `--body` in a single call:

```bash
synapse-cli --json update <id> \
  --title "<concise summary ≤80 chars>" \
  --body "**Original title:** <original verbose title>

<existing body content preserved here>"
```

- Keep the rewritten title actionable and descriptive — summarize intent, don't just truncate
- If the task already has body content, prepend the original title line above it
- Skip if title is already ≤80 chars

### 5. Add brief description if missing

If the task body is empty or has no meaningful context beyond a URL, add a 2-3 sentence description based on what you know from the title, URL context (if fetched), and general understanding. Do NOT explore the codebase or read source files — just clarify what the task is about and what "done" looks like.

```bash
synapse-cli --json update <id> \
  --body "Brief description of what needs to happen and expected outcome.

Original context preserved here if any."
```

Skip if the task already has a clear, descriptive body.

### 6. Assign tags based on analysis

Common tag categories:
- **Domain**: `backend`, `frontend`, `infra`, `docs`, `ci`
- **Size**: `small`, `medium`, `large`
- **Type**: `bug`, `feature`, `refactor`, `review`

```bash
synapse-cli --json update <id> --tags "backend,small,review"
```

### 7. Set agent mode

- `headless` — automated tasks: code reviews, simple fixes, test writing
- `interactive` — tasks needing human guidance: architecture decisions, complex debugging

```bash
synapse-cli --json update <id> --mode headless
```

### 8. Assign project (if applicable)

Check if the task references a known project (GitHub repo). List available projects:

```bash
synapse-cli --json project list
```

If the task body/URL matches a registered project, assign it:

```bash
synapse-cli --json update <id> --project "owner/repo"
```

After assigning, look up the project type:

```bash
synapse-cli --json project get "owner/repo"
```

If `type` is `work`, apply these overrides in the next steps:
- Force `planning` status for medium/large features (regardless of other signals)
- Set mode to `interactive` unless the task is a PR review
- Add note to body: "Work project — enforcing higher standards"

### 9. Decide: planning or direct implementation

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
| **Work project** + size `medium`/`large` + type `feature` | planning (forced) |

Step 8 already sets the status — no further status update needed. Skip if a previous step already changed the status.

## Decision Criteria

| Signal | Mode | Tags |
|--------|------|------|
| PR review URL | headless | review, size based on diff |
| Bug report with repro | headless | bug, domain from stack trace |
| Feature request, unclear scope | interactive | feature, large |
| Simple refactor/rename | headless | refactor, small |
| Architecture decision | interactive | feature, large |
