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
| Agent completed work successfully | in-review | Human must review before done |
| Agent failed, hit errors, or produced no useful output | human-required | Needs human to decide retry |
| Agent explicitly said it's blocked or needs input | human-required | Needs human intervention |

### Guidelines

- **Never set `done`** — only humans move tasks to `done` after review
- **Never set `todo`** — this triggers auto-dispatch and can create duplicate agents
- Default to `in-review` when uncertain
- Set `human-required` if the agent output shows errors, loops, or incomplete work

### 4. Update the task status

```bash
synapse-cli --json update <id> --status <new-status>
```

If relevant, append a brief evaluation note to the task body explaining the decision.
