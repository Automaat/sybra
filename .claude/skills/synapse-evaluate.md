---
name: synapse-evaluate
description: Evaluate completed Synapse tasks — review agent output, determine appropriate status transition. Use when asked to evaluate task completion.
allowed-tools: Bash, Read
user-invocable: true
---

# Synapse Task Evaluation

Evaluate a task after an agent has finished work. Determine the appropriate status transition based on the agent's output.

## Process

### 1. Read the task

```bash
synapse-cli --json get <id>
```

### 2. Analyze the agent result

Review the agent result provided in the prompt. Consider:

- Did the agent complete the work described in the task?
- Are there errors or signs of failure?
- Is the output a partial result that needs review?

### 3. Decide on status transition

| Condition | New Status | Rationale |
|-----------|-----------|-----------|
| Agent completed work successfully, task is simple/mechanical | done | No review needed |
| Agent completed work but output needs human verification | in-review | Complex changes need eyes |
| Agent failed, hit errors, or produced no useful output | todo | Reset for retry |
| Agent explicitly said it's blocked or needs input | blocked | Needs human intervention |

### Guidelines

- Default to `in-review` when uncertain — safer to have a human check
- Only set `done` for clearly successful, low-risk completions (docs, simple refactors, reviews)
- Set `todo` if the agent output shows errors, loops, or incomplete work
- If the task has tags like `large` or `feature`, prefer `in-review` over `done`

### 4. Update the task status

```bash
synapse-cli --json update <id> --status <new-status>
```

If relevant, append a brief evaluation note to the task body explaining the decision.
