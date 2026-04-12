---
name: synapse-self-monitor
description: Periodic meta-analysis of orchestrator + agent runs. Reads session JSONLs, audit logs, and agent ndjson; detects bugs, cost outliers, and error patterns; files deduped GitHub issues with label synapse-self-monitor. Designed for loop-agent invocation every ~6h.
allowed-tools: Bash, Read, Grep, Glob
user-invocable: true
---

# Synapse Self-Monitor

Automated meta-analysis of the Synapse orchestrator and its agents. Scans recent session logs, audit events, and agent output for error patterns, cost outliers, and workflow anomalies. Files deduped GitHub issues for novel findings.

Designed to run as a loop agent (`/synapse-self-monitor` every 6h) but also works as a one-shot invocation.

## When not to use

- Real-time board monitoring → use `/synapse-monitor` (runs every 5m inside the orchestrator session).
- Deep historical audit report → use `/synapse-audit`.
- Triaging tasks → use `/synapse-triage`.

## Hard rules

- **Cap at 5 new issues per run.** Beyond that, stop filing and report the overflow count in the summary. Why: avoids flooding the tracker on a bad day.
- **Never reopen closed issues.** If the same finding matches a closed issue, skip it — the fix may already be deployed. Why: prevents noise loops on known-fixed problems.
- **Always emit a JSON summary line at the end.** Why: the loop agent's audit trail captures the headless `result` event; a parseable summary makes downstream analysis easy.
- **Read-only analysis.** Never modify tasks, agent state, or config. Only side effects: filing GitHub issues. Why: this skill runs outside the orchestrator lifecycle; mutations would race with the active orchestrator session.

## Phase 1 — Gather data (24h lookback)

### Orchestrator session logs

```bash
# Find session JSONLs modified in the last 24h
find ~/.claude/projects/ -name "*.jsonl" -mtime -1 -type f 2>/dev/null
```

For each file, extract tool_result content blocks and scan for error patterns (Phase 2).

### Audit logs

```bash
# Today + yesterday
cat ~/.synapse/logs/audit/$(date -u +%Y-%m-%d).ndjson ~/.synapse/logs/audit/$(date -u -v-1d +%Y-%m-%d).ndjson 2>/dev/null
```

Parse as NDJSON. Build:
- `agent_completions[]` — all `agent.completed` events with role, cost, duration, state, name
- `agent_failures[]` — all `agent.failed` events
- `task_events[]` — all `task.*` events

### Agent output logs (spot-check)

```bash
# Today's agent ndjson files
ls ~/.synapse/logs/agents/*-$(date -u +%Y-%m-%d)*.ndjson 2>/dev/null
```

Only read specific agent logs when investigating a finding from Phase 2 (don't read all — too much data).

## Phase 2 — Detect findings

For each detector, produce a finding with: `category`, `title`, `evidence` (sample log lines), `suggested_fix`.

### 2.1 Unknown skill errors

Scan orchestrator session tool_result blocks for `<tool_use_error>Unknown skill: <name></tool_use_error>`.

- Finding title: `fix(skills): unknown skill <name>`
- Evidence: the full error line + session file path
- Fix: check `~/.synapse/.claude/skills/` for the missing skill; if absent, check `syncSkills()` in app.go

### 2.2 Task parser warnings

Scan orchestrator session tool_results for `task.parse.skip` warnings.

- Finding title: `fix(task): parser rejects <filename>`
- Evidence: the warning line
- Fix: either the file lacks frontmatter (sidecar file leaking into List) or has malformed YAML

### 2.3 Tool use errors (non-skill)

Scan for `<tool_use_error>` patterns that are NOT Unknown skill errors.

- Finding title: `fix(orchestrator): tool error: <short description>`
- Evidence: the error text
- Deduplicate by error text prefix (first 80 chars)

### 2.4 Agent failures

Check audit `agent.failed` events.

- Finding title: `fix(agent): <role> agent failed on <task_id>`
- Evidence: the full audit event JSON
- Group by role — if the same role failed 3+ times, consolidate into one issue

### 2.5 Cost outliers

From `agent_completions`, compute per-role stats:

| Role | Alert threshold |
|------|----------------|
| eval | > $0.50 per run |
| triage | > $0.50 per run |
| implementation | > $15.00 per run |
| pr-fix | > $5.00 per run |
| any | total daily > $200 |

- Finding title: `perf(<role>): cost outlier $<amount> on <task_id>`
- Evidence: the audit event + comparison to role average

### 2.6 Stuck in-progress tasks

From audit, find tasks that entered `in-progress` over 6h ago with no subsequent `agent.completed` event.

- Finding title: `fix(workflow): task <id> stuck in-progress >6h`
- Evidence: the last status change event + current time

### 2.7 Python/script crashes in orchestrator

Scan orchestrator tool_results for `Traceback`, `TypeError`, `SyntaxError`, `NameError`.

- Finding title: `fix(orchestrator): inline script crash: <error type>`
- Evidence: the traceback text

## Phase 3 — Deduplicate against existing issues

Determine the target repo:

```bash
REPO=$(cd ~/.synapse && gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || echo "")
```

If empty, try reading from `~/.synapse/config.yaml` or fall back to the first registered project:

```bash
REPO=$(synapse-cli --json project list 2>/dev/null | jq -r '.[0].id // empty')
```

If still empty, skip issue filing entirely and just print findings to stdout.

Ensure label exists:

```bash
gh label create synapse-self-monitor --repo "$REPO" --color C5DEF5 --description "Filed by /synapse-self-monitor" 2>/dev/null || true
```

For each finding, derive a stable title and search:

```bash
gh issue list --repo "$REPO" --label synapse-self-monitor --state all --search "in:title \"<title prefix>\"" --json number,state,title --limit 3
```

- If an **open** issue matches → skip (already tracked)
- If a **closed** issue matches → skip (already fixed)
- If no match → file new issue (up to the 5-issue cap)

## Phase 4 — File new issues

For each novel finding (up to 5):

```bash
gh issue create --repo "$REPO" --label synapse-self-monitor --title "<title>" --body "$(cat <<'EOF'
## Detection

- **Time:** <ISO timestamp>
- **Source:** <file path or audit event type>
- **Detector:** synapse-self-monitor v1

## Evidence

```
<sample log lines, max 20 lines>
```

## Suggested fix

<1-2 sentences>

## Context

Filed automatically by the synapse-self-monitor loop agent. Review and close when addressed.
EOF
)"
```

## Phase 5 — Summary

Print exactly one JSON line to stdout as the final output:

```json
{"findings": <total detected>, "filed": <new issues created>, "skipped_dup": <matched existing issues>, "skipped_cap": <over the 5-issue cap>, "errors": <analysis errors>}
```

This line is captured by the headless agent's `result` event and persisted in the audit log, making it queryable by `/synapse-audit` and visible in the loop agent's run history.

## Examples

<example>
**Clean run — no findings.**

Orchestrator sessions have no errors. Audit shows 0 failures, all costs within thresholds. No stuck tasks.

Output:
```json
{"findings": 0, "filed": 0, "skipped_dup": 0, "skipped_cap": 0, "errors": 0}
```
</example>

<example>
**Mixed findings with dedup.**

Detects: 2 unknown skill errors (synapse-triage, synapse-monitor), 1 cost outlier (eval at $1.20), 3 task.parse.skip warnings. Existing open issue matches the skill error pattern. Cost outlier and 1 parse warning are novel.

Actions:
- Skip 2 skill errors (open issue exists)
- Skip 2 parse warnings (same root cause, deduplicated to 1)
- File: `perf(eval): cost outlier $1.20 on task-abc`
- File: `fix(task): parser rejects 08d26c47.plan-critique.md`

Output:
```json
{"findings": 4, "filed": 2, "skipped_dup": 2, "skipped_cap": 0, "errors": 0}
```
</example>

<example>
**Many findings, cap applies.**

Detects 8 distinct novel findings. Files the first 5, skips the remaining 3 due to the cap.

Output:
```json
{"findings": 8, "filed": 5, "skipped_dup": 0, "skipped_cap": 3, "errors": 0}
```
</example>

## Errors

| Error | Response |
|---|---|
| No orchestrator session files found | Skip Phase 2.1-2.3, 2.7. Continue with audit-only detectors. |
| Audit log missing | Skip audit-derived detectors (2.4-2.6). Report `errors: 1` in summary. |
| `gh` auth failure or rate limit | Skip Phase 3-4. Print findings to stdout only. Report `errors: 1`. |
| No repo determinable | Skip Phase 3-4. Print findings to stdout only. Not an error (may be expected for local-only setups). |
