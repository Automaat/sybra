---
name: sybra-self-monitor
description: Periodic deep analysis of agent runs and workflow health. Reads structured findings from Go health checker, investigates root causes by reading agent logs, cross-correlates patterns, and files deduped GitHub issues. Designed for loop-agent invocation every ~6h.
allowed-tools: Bash, Read, Grep, Glob
user-invocable: true
---

# Sybra Self-Monitor

Deep reasoning analysis of Sybra agent runs and workflow health. The Go health checker (`internal/health/`) runs every 10 minutes and produces structured findings (cost outliers, failure spikes, stuck tasks, workflow loops, status bounces, cost drift). This skill consumes those findings and investigates the **why** — reading agent conversation logs, cross-correlating patterns, and producing actionable recommendations.

Designed to run as a loop agent (`/sybra-self-monitor` every 6h) but also works as a one-shot invocation.

## When not to use

- Real-time board monitoring → use `/sybra-monitor` (runs every 5m inside the orchestrator session).
- Deep historical audit report → use `/sybra-audit`.
- Triaging tasks → use `/sybra-triage`.

## Hard rules

- **Cap at 5 new issues per run.** Beyond that, stop filing and report the overflow count in the summary.
- **Never reopen closed issues.** If the same finding matches a closed issue, skip it.
- **Always emit a JSON summary line at the end.** Parseable by downstream audit analysis.
- **Read-only analysis.** Never modify tasks, agent state, or config. Only side effects: filing GitHub issues.
- **Be specific.** Don't say "agent was expensive" — say "agent spent 47 turns reading every file in src/ instead of grepping for the symbol."

## Phase 1 — Gather structured health data

```bash
sybra-cli --json health
```

This returns the latest health report from the Go checker with all findings and stats.

If no report exists (app not running), fall back to manual audit analysis:

```bash
sybra-cli --json audit --since 24h --summary
```

For each finding with a `taskId`, fetch task details:

```bash
sybra-cli --json get <taskId>
```

## Phase 2 — Deep investigation

For each finding, investigate the root cause. Prioritize: critical findings first, then warnings.

### Cost outliers

1. Read the agent's NDJSON log file (path in finding's `logFile` or task's `agentRuns[].logFile`)
2. Analyze tool call patterns:
   - Is the agent reading too many files instead of grepping?
   - Is it running expensive bash commands repeatedly?
   - Is it stuck in a reasoning loop (same tool call 3+ times with similar args)?
   - Is the task appropriately sized? (small task shouldn't need $15 of agent time)
3. Check if the prompt/skill being invoked is inefficient

### Agent failures

1. Read failed agent's log to find the actual error
2. Classify: transient (API rate limit, overloaded_error, network) vs systematic (bad prompt, missing context, wrong approach)
3. Cross-reference: do multiple failures involve the same project, same codebase area, or same skill?
4. Check if the failure is a known issue (same error pattern in recent runs)

### Workflow loops (plan-reject cycles)

1. Read the task's plan and plan-critique content
2. Determine: is plan quality poor (agent not reading enough code), or are rejection criteria too strict?
3. Check if the planner and critic are using the same codebase context

### Stuck tasks

1. Check if there's a live agent that simply hasn't reported (watchdog may handle this)
2. Read the last agent's log to understand where progress stalled
3. Check if the task depends on external input (human review, PR merge)

### Status bounces

1. Read task history to understand the back-and-forth
2. Determine: is the workflow definition causing the bounce, or is it agent failures triggering retries?

### Cost drift

1. Compare cost patterns by role — which role is driving the increase?
2. Check if task complexity has changed (more large tasks) or if agents are becoming less efficient
3. Look at recent skill/prompt changes that might affect token usage

## Phase 3 — Cross-correlation

After investigating individual findings, look for patterns across them:

- **Codebase hotspots:** Multiple failures or expensive runs touching the same project or directory?
- **Skill degradation:** Is one skill (triage, plan, eval) responsible for a disproportionate share of findings?
- **Time patterns:** Do problems cluster at certain times? (e.g., rate limits during peak hours)
- **Cascade effects:** Did one failure trigger a chain? (failed impl → stuck task → status bounce)

## Phase 4 — File issues

Determine the target repo:

```bash
REPO=$(cd ~/.sybra && gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || echo "")
```

If empty, try the first registered project:

```bash
REPO=$(sybra-cli --json project list 2>/dev/null | jq -r '.[0].id // empty')
```

If still empty, skip issue filing and just print findings to stdout.

Ensure label exists:

```bash
gh label create sybra-self-monitor --repo "$REPO" --color C5DEF5 --description "Filed by /sybra-self-monitor" 2>/dev/null || true
```

For each confirmed finding (not false positives), derive a stable title and search for duplicates:

```bash
gh issue list --repo "$REPO" --label sybra-self-monitor --state all --search "in:title \"<title prefix>\"" --json number,state,title --limit 3
```

- Open match → skip
- Closed match → skip
- No match → file (up to 5-issue cap)

Issue body format:

```bash
gh issue create --repo "$REPO" --label sybra-self-monitor --title "<title>" --body "$(cat <<'EOF'
## Detection

- **Time:** <ISO timestamp>
- **Category:** <finding category>
- **Severity:** <severity>

## Root cause

<1-3 sentences explaining WHY this happened, based on log analysis>

## Evidence

```
<relevant log excerpts, max 20 lines — tool calls, errors, patterns found>
```

## Recommended action

<specific, actionable recommendation — not "investigate further" but "update the eval skill prompt to grep for changed files instead of reading the full tree">

## Context

Filed by sybra-self-monitor. Findings from Go health checker investigated with agent log analysis.
EOF
)"
```

## Phase 5 — Summary

Print exactly one JSON line to stdout as the final output:

```json
{"findings_total": 5, "investigated": 5, "confirmed": 3, "false_positives": 2, "filed": 2, "skipped_dup": 1, "skipped_cap": 0, "errors": 0}
```

## Examples

<example>
**Clean run — no findings from health checker.**

Health report shows 0 findings. Stats look normal.

Output:
```json
{"findings_total": 0, "investigated": 0, "confirmed": 0, "false_positives": 0, "filed": 0, "skipped_dup": 0, "skipped_cap": 0, "errors": 0}
```
</example>

<example>
**Cost outlier investigated — false positive.**

Health checker flags eval agent at $0.65 (threshold $0.50). Reading the log reveals the task was a large PR review (800 lines changed) — the cost is proportional to the work. Mark as false positive, don't file.

Output:
```json
{"findings_total": 1, "investigated": 1, "confirmed": 0, "false_positives": 1, "filed": 0, "skipped_dup": 0, "skipped_cap": 0, "errors": 0}
```
</example>

<example>
**Multiple failures cross-correlated.**

Health checker flags 3 agent failures + high failure rate. Reading logs reveals all 3 failed on the same project with `permission denied` errors on worktree paths. Root cause: stale worktree not cleaned up. File one consolidated issue instead of three.

Actions:
- Investigate 4 findings (3 failures + 1 failure_rate)
- Confirm 1 root cause (stale worktree)
- File 1 issue: `fix(worktree): stale worktree blocking agents on project X`

Output:
```json
{"findings_total": 4, "investigated": 4, "confirmed": 4, "false_positives": 0, "filed": 1, "skipped_dup": 0, "skipped_cap": 0, "errors": 0}
```
</example>

## Errors

| Error | Response |
|---|---|
| No health report and audit unavailable | Report `errors: 1` in summary. Skip investigation. |
| Agent log file missing or unreadable | Note in investigation, don't fail the whole run. Continue with other findings. |
| `gh` auth failure or rate limit | Skip Phase 4. Print findings to stdout only. Report `errors: 1`. |
| No repo determinable | Skip Phase 4. Print findings to stdout only. Not an error. |
