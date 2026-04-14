---
name: synapse-audit
description: Analyze Synapse health findings + audit summary to explain workflow issues and propose grounded fixes. Use when asked to review how work is flowing or suggest process improvements.
allowed-tools: Bash, Read
user-invocable: true
---

# Synapse Audit Analysis

The Go side already runs every detector you would otherwise reproduce here:
failure rate, cost outliers (per-role), stuck tasks, plan-rejection loops,
status bounce, cost drift, agent retry loops, triage mode mismatches, and
status bottlenecks. Your job is to **read the findings + summary, ground them
in the actual tasks, and propose concrete fixes** — not to recompute them.

## Step 1: Pull the pre-computed health report

```bash
synapse-cli --json health
```

Returns a `Report` with:
- `score` — `good` | `warning` | `critical` (rollup across all findings)
- `findings[]` — each has `category`, `severity`, `title`, `description`,
  `taskId`, `agentId`, `role`, `evidence`, `detectedAt`
- `stats` — totals, failure rate, cost by role

Categories you may see and what each means:
- `failure_rate` — too many agent runs failed today
- `cost_outlier` — a single run exceeded the per-role budget
- `cost_drift` — today's avg cost is way above the 7-day rolling avg
- `stuck_task` — a task has been in-progress >6h with no agent activity
- `workflow_loop` — a task hit 3+ plan rejections (triage→plan→reject loop)
- `status_bounce` — task ping-ponged the same status transition 2+ times
- `agent_retry_loop` — same task accumulated 2+ failed agent runs
- `triage_mismatch` — task triaged as headless that escalated to human-required
- `status_bottleneck` — average dwell in a status exceeded its threshold

## Step 2: Pull the workflow summary

```bash
synapse-cli --json audit --since 7d --summary
```

Use this for trend context: cycle time, total cost, plan rejection rate,
status bottleneck dwell hours over the longer window. Don't recompute these.

## Step 3: Ground each finding

For the top 3 findings (by severity, then by impact), read the actual task so
your suggestions cite real content, not generic advice:

```bash
synapse-cli --json get <task_id>
```

Use the task body, status_reason, plan, and any embedded run results to
explain *why* the finding fired and what specifically should change.

## Step 4: Produce the report

```
Health: <report.score>
Period: <report.periodStart> → <report.periodEnd>

Findings: <N> (critical: <C>, warning: <W>)

Top issues:
1. [category] <title>
   - Task: <task_id> — <one-line task summary from the task body>
   - Why it fired: <evidence-grounded explanation>
   - Fix: <specific, actionable change — prompt diff, mode flip, triage
     rule update, subtask split — referencing real task content>
2. ...
3. ...

Recommendations (cross-cutting):
- <patterns spanning multiple findings: e.g., "3 of 5 retry loops are eval
  agents — the eval prompt template needs a verification step">
- <triage rule changes grounded in triage_mismatch findings>
- <cost optimizations grounded in cost_outlier evidence>
```

## Rules

- **Never recompute what the report gives you.** If you find yourself filtering
  raw audit events to count failures or measuring dwell times, stop — those
  are already in `findings` and `stats`.
- **Always read the underlying task** for the top findings before suggesting a
  fix. Generic advice ("split into smaller subtasks") is not useful.
- **Cite evidence values** from `finding.evidence` in your explanation so the
  user can verify the call.
- **Skip the report entirely** if `score == "good"` and no findings — output
  one line: `Health: good — no findings in the last 24h.`

<example>
`synapse-cli --json health` returns:
```json
{
  "score": "critical",
  "findings": [
    {"category": "agent_retry_loop", "severity": "critical", "taskId": "task-abc123",
     "title": "task task-abc123 has 3 failed agent runs",
     "evidence": {"failure_count": 3}},
    {"category": "triage_mismatch", "severity": "warning", "taskId": "task-def456",
     "title": "task task-def456 triaged headless but escalated to human-required"}
  ],
  "stats": {"totalAgentRuns": 18, "failedAgentRuns": 5, "failureRate": 0.28}
}
```

Then `synapse-cli --json get task-abc123` shows a task asking the agent to
refactor a 2000-line file. Output:

```
Health: critical
Period: 2026-04-13 → 2026-04-14

Findings: 2 (critical: 1, warning: 1)

Top issues:
1. [agent_retry_loop] task task-abc123 has 3 failed agent runs
   - Task: refactor internal/agent/manager.go (~2000 LOC)
   - Why it fired: 3/3 headless attempts failed; file is too large for
     a single agent context.
   - Fix: split into 3 subtasks scoped per concern (lifecycle, persistence,
     events), or flip to interactive mode so a human can guide the boundaries.

2. [triage_mismatch] task task-def456 triaged headless but escalated to human-required
   - Task: integrate new auth provider (touches secrets + IAM)
   - Why it fired: triage saw "integrate" + small description and chose
     headless; task actually needs credential setup that no agent can do.
   - Fix: add triage rule "tasks tagged `secrets` or `iam` → interactive".

Recommendations:
- Add a triage heuristic: tasks targeting files >1000 LOC default to interactive
  or get auto-split before dispatch.
- Eval-agent failure rate (28%) is approaching the 30% threshold — review the
  last week of eval prompts for common failure modes.
```
</example>

<example>
`synapse-cli --json health` returns `{"score": "good", "findings": []}`.

Output: `Health: good — no findings in the last 24h.`
</example>
