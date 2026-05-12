---
name: plan-fork
description: Merge two independent implementation plan drafts into one clean Sybra plan. Use when given two plan file paths and an optional --task/--out.
allowed-tools: Read, Write, Bash, Skill
user-invocable: true
---

# Plan Fork

Synthesize two independent plan drafts into one canonical implementation plan.

You are a convergence step. Do not implement code. Do not modify task status.

## Inputs

Invocation shape:

```bash
/plan-fork <plan-a-path> <plan-b-path> --task <task-id> --out <output-path>
$plan-fork <plan-a-path> <plan-b-path> --task <task-id> --out <output-path>
```

- First two positional arguments are draft markdown files.
- `--task` is the canonical Sybra task ID.
- `--out` is where to write the synthesized plan. If omitted, print the plan.

## Process

1. Read both draft files fully.
2. Verify both drafts target the `--task` ID when a task ID is provided.
3. Detect contamination:
   - other Sybra task IDs
   - unrelated branch names
   - unrelated worktree paths
   - stale PR, issue, ticket, or repo references not present in both drafts or task context
4. Drop contaminated details. Keep only evidence-backed implementation content.
5. Merge the strongest parts into one plan:
   - scope and goal
   - touched files and symbols
   - ordered implementation steps
   - verification commands and expected outcomes
   - risks and rollback notes when relevant
6. Resolve conflicts explicitly by choosing the simpler, safer path. Do not preserve both alternatives unless the human must decide.
7. Critique the synthesized draft:
   - Claude: invoke `/plan-critic` if available.
   - Codex: invoke `$plan-critic` if available.
   - If unavailable, do a local critique pass with the same standard.
   Address concrete defects in the final plan.
8. Write only the final plan markdown to `--out` when provided.

## Output Format

```markdown
## Goal

<one concise paragraph>

## Scope

- <included work>
- <excluded work if important>

## Implementation

1. <step>
   - Files: `<path>`
   - Details: <specific symbols/changes>
   - Verify: `<command>` -> <expected result>

## Risks

- <risk or "None">

## Open Questions

- <question or "None">
```

## Rules

- No placeholders.
- No TODOs.
- No unrelated refactors.
- No task status changes.
- No direct `sybra-cli update`.
- No branch/worktree/task IDs except the canonical `--task` value.
