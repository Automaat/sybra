---
name: sybra-audit
description: Analyze Sybra health findings + audit summary to explain workflow issues and propose grounded fixes. Use when asked to review how work is flowing or suggest process improvements.
allowed-tools: Bash, Read
user-invocable: true
---

# Sybra Audit Analysis

The Go side already runs every detector and, when the selfmonitor service is
enabled, distills each finding's agent log into a `LogSummary` (stall
detection, repeated calls, error classes, cost attribution) and classifies it
with an LLM judge. Your job is to **read those pre-computed results, ground
them in real context, and produce concrete fixes** — not to recompute them.

## Step 1: Pull the selfmonitor report

```bash
sybra-cli --json selfmonitor scan
```

Returns a `selfmonitor.Report` with:
- `healthScore` — `good` | `warning` | `critical`
- `findings[]` — each `InvestigatedFinding` has:
  - `.finding` — `category`, `severity`, `title`, `evidence`, `taskId`
  - `.logSummary` — `stallDetected`, `stallReason`, `repeatedCalls`,
    `errorClasses`, `inferredCostPerTool`, `lastToolCalls`, `totalCostUsd`
  - `.verdict` — `classification`, `rootCause`, `confidence`, `nextAction`
    (may be `"pending"` if judge is disabled or no log file resolved)
- `correlations[]` — `kind` (`same_project` | `same_error_class` | `cascade`),
  `key`, `count`, `fingerprints`, `description`

**Fallback** — if the report is missing or `generatedAt` is > 2h ago:

```bash
sybra-cli --json health
```

## Step 2: Pull the workflow summary

```bash
sybra-cli --json audit --since 7d --summary
```

Use for trend context only: cycle time, total cost, plan rejection rate, status
bottleneck dwell over the longer window.

## Step 3: Ground each finding

For the top 3 findings (by severity, then impact):

**If `logSummary` is present:**
- Cite `stallReason` directly when `stallDetected: true` — no need to read task
- Cite the top `repeatedCalls` entry (tool name + count + sampleInput) for tool-loop findings
- Cite `errorClasses[0]` (class + sample) for failure findings
- Use `inferredCostPerTool` to name the expensive tool for cost outliers

**If `verdict.classification != "pending"`:**
- Use `verdict.rootCause` as the primary diagnosis
- Use `verdict.nextAction` as the fix — only refine if task content adds nuance

**Otherwise (no logSummary, verdict pending):**

```bash
sybra-cli --json get <taskId>
```

Read task body, status_reason, and any embedded run results to explain why
the finding fired.

## Step 4: Produce the report

```
Health: <healthScore>
Period: <periodStart> → <periodEnd>

Findings: <N> (critical: <C>, warning: <W>)
{{if correlations}}
Correlations: <K>
{{end}}

Top issues:
1. [category] <title>
   - Task: <taskId> — <one-line task summary>
   - Root cause: <verdict.rootCause OR logSummary-derived explanation>
   - Evidence: <specific signal — stallReason, repeatedCalls[0], errorClasses[0].sample>
   - Fix: <verdict.nextAction OR specific actionable change>

2. ...
3. ...

{{if correlations}}
Correlations:
- [<kind>] <description>
{{end}}

Recommendations (cross-cutting):
- <patterns spanning multiple findings or correlations>
- <triage rule changes grounded in triage_mismatch findings>
- <cost optimizations grounded in cost_outlier evidence>
```

## Rules

- **Never recompute what the report gives you.** If you find yourself filtering
  raw audit events, stop — use findings and stats.
- **Cite log-summary signals by name.** Say "stall: last 5 calls are identical
  Read invocations" not "the agent seemed stuck."
- **Skip the report entirely** if `healthScore == "good"` and no findings:
  output one line: `Health: good — no findings in the last 24h.`
- **Trust the verdict when confidence ≥ 0.8.** Confirm with task content only
  when confidence is lower or verdict is pending.

<example>
`sybra-cli --json selfmonitor scan` returns:
```json
{
  "healthScore": "critical",
  "findings": [
    {
      "finding": {"category": "agent_retry_loop", "severity": "critical",
                  "taskId": "task-abc123", "title": "task task-abc123 has 3 failed agent runs",
                  "evidence": {"failure_count": 3}},
      "logSummary": {
        "stallDetected": true,
        "stallReason": "last 5 tool calls are identical Read(path='internal/agent/manager.go') invocations",
        "totalCostUsd": 0.43,
        "inferredCostPerTool": {"Read": 0.38, "Bash": 0.05}
      },
      "verdict": {"classification": "confirmed", "confidence": 0.92,
                  "rootCause": "agent loops on reading the same 2000-line file without making progress",
                  "nextAction": "split task into 3 subtasks scoped per concern, or flip to interactive mode"}
    },
    {
      "finding": {"category": "triage_mismatch", "severity": "warning",
                  "taskId": "task-def456", "title": "task task-def456 triaged headless but escalated to human-required",
                  "evidence": {"classified_mode": "headless", "final_status": "human-required"}},
      "logSummary": null,
      "verdict": {"classification": "pending"}
    }
  ],
  "correlations": []
}
```

Then for the pending triage_mismatch, `sybra-cli --json get task-def456` shows
a task about integrating a new auth provider (touches secrets + IAM).

Output:

```
Health: critical
Period: 2026-04-13 → 2026-04-14

Findings: 2 (critical: 1, warning: 1)

Top issues:
1. [agent_retry_loop] task task-abc123 has 3 failed agent runs
   - Task: refactor internal/agent/manager.go (~2000 LOC)
   - Root cause: agent loops on reading the same 2000-line file without making progress
   - Evidence: stall — last 5 calls are identical Read('internal/agent/manager.go');
     78% of $0.43 cost attributed to Read
   - Fix: split into 3 subtasks scoped per concern, or flip to interactive mode

2. [triage_mismatch] task task-def456 triaged headless but escalated to human-required
   - Task: integrate new auth provider (touches secrets + IAM)
   - Root cause: triage saw "integrate" + small description and chose headless;
     task needs credential setup no agent can do autonomously
   - Evidence: classified_mode=headless → final_status=human-required
   - Fix: add triage rule "tasks tagged secrets or iam → interactive"

Recommendations:
- Add triage heuristic: tasks targeting files >1000 LOC default to interactive
  or get auto-split before dispatch.
- The selfmonitor actor (when dry_run=false) will auto-flip task-def456 to
  interactive on the next selfmonitor tick.
```
</example>

<example>
`sybra-cli --json selfmonitor scan` returns `{"healthScore": "good", "findings": []}`.

Output: `Health: good — no findings in the last 24h.`
</example>
