---
name: sybra-plan
description: Plan Sybra tasks — analyze scope, explore codebase, produce implementation plan without writing code. Use when asked to plan a task.
allowed-tools: Bash, Read, Glob, WebFetch
user-invocable: true
---

# Sybra Task Planning

Produce a detailed implementation plan for a task. Do NOT implement, write code, create files, or make changes.

You run inside an interactive tmux session. After producing a plan you STAY at the prompt and wait for feedback from the user — you never exit.

## CLI Reference

The ONLY valid flags for `sybra-cli update` are: `--title`, `--status`, `--body`, `--plan`, `--plan-file`, `--mode`, `--tags`, `--project`. Do NOT use any other flag.

## Process

### 1. Read the task

```bash
sybra-cli --json get <id>
```

### 2. Analyze scope

- Read the task body, understand what's being asked
- If URLs are referenced, fetch context (GitHub PRs/issues via `gh`, or WebFetch)
- Explore the codebase: find relevant files, understand existing patterns
- Identify dependencies and potential risks

### 3. Produce a structured plan

Output a markdown plan with these sections:

```markdown
## Approach

Brief description of the chosen approach and why.

## Files to Change

- `path/to/file.go` — what changes and why
- `path/to/other.go` — what changes and why

## Steps

1. First step — details
2. Second step — details
3. ...

## Risks

- Risk 1 and mitigation
- Risk 2 and mitigation
```

### 4. Publish the plan + hand off for review

```bash
sybra-cli --json update <id> --plan "<full plan markdown>"
sybra-cli --json update <id> --status plan-review
```

Then STOP and wait at the chat prompt. Do NOT exit. Do NOT implement.

### 5. Respond to feedback

The user may send feedback in the same chat session. When feedback arrives:

1. Read it carefully
2. Revise the plan (use prior context — do not re-analyze files you already read)
3. `sybra-cli --json update <id> --plan "<revised plan>"`
4. `sybra-cli --json update <id> --status plan-review`
5. Wait again

### Guidelines

- Be specific: name files, functions, types
- Keep it actionable — each step should be implementable
- Note existing patterns to follow
- Flag anything ambiguous that needs human input
- Do NOT write code, create files, or make any changes
- Do NOT exit after publishing the plan — keep the session alive for review rounds

<example>
Input: Task `task-abc` body: "Add rate limiting to /api/login endpoint, 5 req/min per IP".

Output plan:
```markdown
## Approach
Middleware using token bucket keyed by client IP. Reuse existing `internal/middleware` package.

## Files to Change
- `internal/middleware/ratelimit.go` — new, token bucket implementation
- `internal/middleware/ratelimit_test.go` — new, table-driven tests
- `cmd/api/main.go` — wire middleware into `/api/login` route

## Steps
1. Add `golang.org/x/time/rate` to go.mod
2. Implement `NewIPLimiter(rps, burst)` returning `func(http.Handler) http.Handler`
3. Write tests covering allow/deny/reset scenarios
4. Wire into login route in main.go

## Risks
- Memory growth from IP map — mitigate with LRU eviction (cap 10k entries)
- Behind proxy: read `X-Forwarded-For` header, trust only proxy range
```

Then:
```bash
sybra-cli --json update task-abc --plan "<plan above>"
sybra-cli --json update task-abc --status plan-review
```
Stay at prompt, wait for feedback.
</example>

<example>
Input: User feedback "also add metrics for blocked requests".

Action: Revise Steps section to add prometheus counter, re-publish plan, set status plan-review, wait again. Do NOT re-read files already explored.
</example>
