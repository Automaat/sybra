---
name: synapse-plan
description: Plan Synapse tasks — analyze scope, explore codebase, produce implementation plan without writing code. Use when asked to plan a task.
allowed-tools: Bash, Read, Grep, Glob, WebFetch
user-invocable: true
---

# Synapse Task Planning

Produce a detailed implementation plan for a task. Do NOT implement, write code, create files, or make changes.

## CLI Reference

The ONLY valid flags for `synapse-cli update` are: `--title`, `--status`, `--body`, `--mode`, `--tags`, `--project`. Do NOT use any other flag.

## Process

### 1. Read the task

```bash
synapse-cli --json get <id>
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

### Guidelines

- Be specific: name files, functions, types
- Keep it actionable — each step should be implementable
- Note existing patterns to follow
- Flag anything ambiguous that needs human input
- Do NOT write code, create files, or make any changes
- Do NOT modify the task via synapse-cli — the system handles status transitions
