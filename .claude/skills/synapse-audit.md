---
name: synapse-audit
description: Analyze Synapse audit logs to identify workflow bottlenecks, failure patterns, cost outliers, and suggest improvements. Use when asked to review how work is flowing or suggest process improvements.
allowed-tools: Bash, Read
user-invocable: true
---

# Synapse Audit Analysis

Analyze audit log data to identify patterns, bottlenecks, and improvement opportunities.

## Step 1: Get the summary

```bash
synapse-cli --json audit --since 7d --summary
```

Review key metrics:
- **failure_rate** > 0.2 → investigate failing agents
- **plan_rejection_rate** > 0.3 → triage is under-specifying tasks
- **avg_cycle_time_hours** → compare across weeks for trends
- **status_bottlenecks_hours** → which statuses have the longest dwell time

## Step 2: Investigate bottlenecks

If `plan-review` or `human-required` have high dwell times:
```bash
synapse-cli --json audit --since 7d --type task.status_changed
```

Look for tasks stuck waiting for human action. Suggest:
- Auto-approve plans for small/simple tasks
- Set up notifications for plan-review tasks

## Step 3: Analyze failures

```bash
synapse-cli --json audit --since 7d --type agent.failed
```

Look for patterns:
- Same task failing multiple times → needs different approach or mode change
- High cost on failed agents → prompt needs refinement
- Specific task types failing more → triage rules need adjustment

## Step 4: Cost analysis

```bash
synapse-cli --json audit --since 7d --type agent.completed
```

Check for:
- Cost outliers (agents spending much more than average)
- Triage/eval agents costing more than expected
- Correlation between task size tags and actual cost

## Step 5: Triage accuracy

Compare triage decisions with outcomes:
```bash
synapse-cli --json audit --since 7d --type triage
synapse-cli --json audit --since 7d --type task.status_changed
```

Check if tasks triaged as `headless` ended up needing `interactive` (escalated to human-required).

## Output format

Produce a concise report with:
1. **Health score** (good/warning/critical) based on failure rate + bottlenecks
2. **Top 3 issues** with specific task IDs
3. **Recommendations** — concrete, actionable changes to triage rules, prompts, or workflow

<example>
Input: `synapse-cli --json audit --since 7d --summary` returns `{"failure_rate": 0.35, "plan_rejection_rate": 0.12, "avg_cycle_time_hours": 8.2, "status_bottlenecks_hours": {"plan-review": 22.5, "in-progress": 3.1}}`

Output:
```
Health: warning
Top issues:
1. failure_rate 0.35 exceeds 0.2 threshold — task-abc123, task-def456 failed 3x each
2. plan-review dwell 22.5h — tasks task-ghi789 stuck waiting human approval
3. task-jkl012 cost $1.80 (10x avg) — prompt needs refinement
Recommendations:
- Auto-approve plans for tasks tagged `small`
- Add slack notification on plan-review transition
- Split task-jkl012 into smaller subtasks
```
</example>

<example>
Input: `synapse-cli --json audit --since 7d --type agent.failed` returns 5 events, 3 from same task-xyz retrying.

Output: Recommendation — switch task-xyz to `interactive` mode, retry loop indicates headless can't resolve the blocker.
</example>
