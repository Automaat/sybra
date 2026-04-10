---
name: synapse-monitor
description: Periodic orchestrator monitor. Checks board state, audit logs, workflow health; auto-remediates drift; files GitHub issues for Synapse app bugs and workflow anomalies. Invoke every ~5 min via /loop 5m /synapse-monitor.
allowed-tools: Bash, Read
user-invocable: true
---

# Synapse Monitor

One pass of the orchestrator monitor loop. Read the board, scan the audit window, detect drift against workflow rules, auto-remediate the idempotent cases, and file deduped GitHub issues for Synapse app bugs and workflow anomalies.

Designed for `/loop 5m /synapse-monitor`.

## When not to use

- One-shot analysis of audit logs → use [/synapse-audit](../../.claude/skills/synapse-audit.md) instead (deeper report, no side effects).
- Triaging new tasks → use `/synapse-triage`. This skill treats `untriaged todo` as a flag, not a task to fix.
- Manual intervention on a single task → edit via `synapse-cli update` directly.

## Before starting (required reads)

Read [orchestrator/CLAUDE.md](../../orchestrator/CLAUDE.md) in full before Phase 3. It defines the workflow rules this monitor enforces: status transitions, dispatch limits, escalation criteria, PR gap handling, and the eval-agent verification block used for PR recovery. Monitor remediations must match that file — not reinterpret it.

## Hard rules

Each rule has a **Why** so edge cases are easy to judge.

- **Dedup every issue.** Always `gh issue list --search` before `gh issue create`.
  **Why:** monitor runs every 5 min, so any un-deduped issue creator would spam the tracker within an hour.
- **Do not re-dispatch failed agents automatically.** File an issue and leave the task alone.
  **Why:** the orchestrator escalation rules in [orchestrator/CLAUDE.md](../../orchestrator/CLAUDE.md) specify a 2-failure human threshold; auto-retry would bypass it.
- **Leave `done` tasks untouched.**
  **Why:** a completed task has no valid transitions in the state machine, and touching it pollutes audit history.
- **Every `synapse-cli update` must include `--status-reason`.**
  **Why:** `--status-reason` is the only way monitor actions appear in audit logs; silent updates make drift invisible to the next cycle.
- **Emit the summary line in every cycle, even on partial failure.**
  **Why:** `/loop` uses stdout lines as cycle markers; a missing summary looks like a hang and trips the user's attention.
- **Non-zero `synapse-cli` exit → file a Synapse app bug issue, then abort the cycle.**
  **Why:** the CLI is the only board-state oracle; continuing on stale data would compound errors.

## Phase 1 — Snapshot board state

```bash
synapse-cli --json list
```

Parse counts by status. Compute:

- `in_progress_count` — if > 3, flag `over_dispatch_limit` (log + issue only, no remediation).
- Any task in `plan-review` or `human-required` with `now - updatedAt > 8h` → flag `stuck_human_blocked`.
- Any `todo` task with empty `tags` or empty `agentMode` → flag `untriaged`.
- Any `in-review` task with `projectId` set but empty `prNumber` → flag `pr_gap`.
- Record every `in-progress` task id for the Phase 2 cross-check.

## Phase 2 — Snapshot audit window

```bash
synapse-cli --json audit --since 15m
synapse-cli --json audit --since 15m --type agent.failed
synapse-cli --json audit --since 1h --summary
```

From the 15-min feed build a set of task ids that emitted `agent.started` or any `agent.*` event. Every `in-progress` task from Phase 1 **not** in this set → flag `lost_agent`.

From the 1h summary read `failure_rate`. If `> 0.3` → flag `failure_spike`.

From `status_bottlenecks_hours`, any status exceeding these thresholds → flag `bottleneck`:

| Status | Max dwell |
|---|---|
| `plan-review` | 4h |
| `human-required` | 8h |
| `in-progress` | 6h |
| other | 12h |

## Phase 3 — Compute stuck tasks by size tag

Before this phase, re-read [orchestrator/CLAUDE.md](../../orchestrator/CLAUDE.md) if not already loaded — size-based dispatch thresholds and escalation criteria live there.

For every non-`done` task compute dwell = `now - updatedAt`. Map size tag to max dwell:

| Size tag | Max dwell |
|---|---|
| `small` | 90m |
| `medium` | 6h |
| `large` | 18h |
| none | 12h |

Exceeding → flag `dwell_exceeded`.

## Phase 4 — Auto-remediate (idempotent actions only)

Apply in order. Each action logs a one-liner for the final summary.

| Flag | Action |
|---|---|
| `untriaged` | Leave status unchanged. `synapse-cli --json update <id> --status-reason "monitor: awaiting triage"`. Do not dispatch. |
| `lost_agent` | `synapse-cli --json update <id> --status todo --status-reason "monitor: agent lost, resetting"` |
| `pr_gap` | Follow the eval-agent verification block in [orchestrator/CLAUDE.md](../../orchestrator/CLAUDE.md): `cd` to the worktree, `git push -u origin HEAD`, `gh pr create --base main --title "..." --body "..."`. On success: `synapse-cli --json update <id> --pr <num> --status-reason "monitor: created missing PR"`. |
| `dwell_exceeded` | `synapse-cli --json update <id> --status human-required --status-reason "monitor: dwell exceeded size tag budget"` |
| `stuck_human_blocked` | No status change — escalate via an issue in Phase 5. |
| `over_dispatch_limit` | No action — escalate via an issue in Phase 5. |
| `failure_spike` | No auto-retry — escalate via an issue in Phase 5. |
| `bottleneck` | No action — escalate via an issue in Phase 5. |

## Phase 5 — File GitHub issues (deduped)

Determine the target repo once per cycle:

```bash
REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)
```

Ensure labels exist (idempotent — safe to run every cycle):

```bash
gh label create monitor --color BFD4F2 --description "Opened by /synapse-monitor" 2>/dev/null || true
gh label create bug --color D73A4A --description "Something isn't working" 2>/dev/null || true
```

### Categories and fingerprints

| Category | Trigger | Title prefix | Fingerprint (dedup key) |
|---|---|---|---|
| Synapse app bug | `synapse-cli` non-zero exit, panic in audit, malformed NDJSON | `[monitor] synapse bug: <short>` | short description text |
| Workflow anomaly | `failure_spike`, `over_dispatch_limit`, `bottleneck`, `stuck_human_blocked` | `[monitor] workflow: <short>` | anomaly kind (e.g. `failure-spike`, `bottleneck-plan-review`) |
| PR gap recurring | Task hit `pr_gap` on two consecutive monitor runs (auto-remediation failed) | `[monitor] pr-gap: <task-id>` | task id |

### Dedup protocol

For every candidate issue:

```bash
EXISTING=$(gh issue list --state open --search "in:title \"[monitor] <fingerprint>\"" --json number,title --limit 1)
```

- Non-empty → `gh issue comment <num> --body "..."` with a new occurrence entry. Skip `gh issue create`.
- Empty → `gh issue create --title "[monitor] ..." --body "..." --label monitor,bug`.

### Issue body template (HEREDOC)

```markdown
## Detection
- Time: <ISO timestamp>
- Monitor cycle: <fingerprint>

## Evidence
<relevant audit event JSON excerpts, one per line>

## Affected tasks
- <task-id>: <title> (<status>)

## Suggested investigation
<one or two bullets pointing at the likely root cause or the orchestrator rule violated>
```

Comment body for dedup hits:

```markdown
## New occurrence at <ISO timestamp>
<fresh evidence JSON>
```

## Phase 6 — Summary line

Emit exactly one line to stdout at the end of the cycle:

```
monitor: new=<n> todo=<n> in-progress=<n> in-review=<n> done=<n> | drift=<n> | remediated=<n> | issues=<opened>/<updated>
```

## Examples

<example>
**Clean cycle — no drift.**

Input: `synapse-cli --json list` returns 2 new, 4 todo, 2 in-progress, 1 in-review, 120 done. All `in-progress` tasks show `agent.started` in the 15-min audit window. `failure_rate` = 0.05. No bottlenecks.

Output:
```
monitor: new=2 todo=4 in-progress=2 in-review=1 done=120 | drift=0 | remediated=0 | issues=0/0
```

No `synapse-cli update` calls, no `gh issue` calls.
</example>

<example>
**Drift + auto-remediate — lost agent and PR gap.**

Input: board shows `task-abc` in `in-progress` with no matching `agent.*` event in the last 15 min. Separately, `task-def` is in `in-review` with `projectId: owner/repo` but empty `prNumber`, and its worktree has commits ahead of `origin/main`.

Actions:
1. `synapse-cli --json update task-abc --status todo --status-reason "monitor: agent lost, resetting"`
2. In task-def worktree: `git push -u origin HEAD` → `gh pr create ...` → captured PR #412
3. `synapse-cli --json update task-def --pr 412 --status-reason "monitor: created missing PR"`

Output:
```
monitor: new=0 todo=3 in-progress=2 in-review=1 done=120 | drift=2 | remediated=2 | issues=0/0
```
</example>

<example>
**Recurring anomaly — dedup comment, no new issue.**

Input: this is the third consecutive cycle detecting `failure_rate = 0.42` (above the 0.3 spike threshold). A previous cycle already opened issue #87 `[monitor] workflow: failure-spike`.

Dedup lookup:
```bash
gh issue list --state open --search "in:title \"[monitor] workflow: failure-spike\"" --json number,title --limit 1
# → [{"number":87,"title":"[monitor] workflow: failure-spike"}]
```

Action: `gh issue comment 87 --body "## New occurrence at 2026-04-10T14:35:00Z ..."`. No new issue created.

Output:
```
monitor: new=1 todo=5 in-progress=3 in-review=2 done=120 | drift=1 | remediated=0 | issues=0/1
```
</example>

## Errors

| Error | Response |
|---|---|
| `synapse-cli` non-zero exit in any phase | File a `[monitor] synapse bug: <cmd>` issue (deduped), emit summary line with `issues=1/0` or `0/1`, then abort the cycle. |
| `synapse-cli` output not valid JSON | Same as above — treat as corrupt oracle. |
| `gh` rate limit (HTTP 403 or `API rate limit exceeded`) | Skip issue creation for this cycle, still emit summary with `issues=0/0`, append `(gh rate limited)` to the summary line. Next cycle retries. |
| `gh repo view` fails (no origin configured) | Skip Phase 5 entirely, still run Phases 1-4, emit summary with `issues=skipped`. |
| Worktree path missing during `pr_gap` remediation | Demote to `stuck_human_blocked` for that task, continue the cycle. |
| Audit log file missing / empty | Treat audit-derived flags (`lost_agent`, `failure_spike`, `bottleneck`) as absent. Board-derived flags still apply. |

## Reference

- Workflow rules, dispatch limits, escalation criteria: [orchestrator/CLAUDE.md](../../orchestrator/CLAUDE.md)
- Audit event types: [internal/audit/audit.go](../../internal/audit/audit.go)
- `synapse-cli` flags: [cmd/synapse-cli/main.go](../../cmd/synapse-cli/main.go)

## How to start the loop

From inside a Claude Code session at the repo root:

```
/loop 5m /synapse-monitor
```

Runs until the session ends or `/loop` is cancelled.
