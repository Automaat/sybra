---
name: synapse-evaluate
description: Evaluate completed Synapse tasks — determine status transition and link PRs. Use when asked to evaluate task completion.
allowed-tools: Bash
user-invocable: true
---

# Synapse Task Evaluation

Decide what happens to a task after an agent finishes. Do NOT read source code, review diffs, or explore the codebase.

## Constraints

- Do NOT use Read tool or explore any files
- Do NOT review code quality, style, or correctness
- Only analyze the agent result text provided in the prompt
- Keep total cost under $0.02 per evaluation

## Process

### 1. Read the task

```bash
synapse-cli --json get <id>
```

### 2. Link PR if created

Search the agent result for PR references. Look for:
- `gh pr create` output containing a URL like `https://github.com/.../pull/N`
- Mentions of "PR #N" or "pull request #N"
- Branch names pushed (for branch linking)

If found, link to task:

```bash
# Link PR number
synapse-cli --json update <id> --pr <number>

# Link branch if known and not already set
synapse-cli --json update <id> --branch <branch-name>
```

### 3. Decide status transition

Based ONLY on the agent result text:

| Condition | New Status |
|-----------|-----------|
| Agent completed work, PR created or code pushed | in-review |
| Agent completed but no PR/push (partial work) | human-required |
| Agent failed, hit errors, looped | human-required |
| Agent blocked, needs input | human-required |

### Rules

- **Never set `done`** — only humans do that
- **Never set `todo`** — triggers auto-dispatch, creates duplicate agents
- Default to `in-review` when uncertain
- Set `human-required` if agent output shows errors, loops, or incomplete work

### 4. Update status

```bash
synapse-cli --json update <id> --status <new-status>
```
