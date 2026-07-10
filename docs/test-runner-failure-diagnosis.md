# Test-Runner Failure Diagnosis (2026-07)

Diagnosis for #1539. Sampled every `test-runner` run recorded with
`outcome: failed` in `~/.sybra/stats.json` for the 2026-07-01..07-06 window
(41 runs across 9 distinct tasks, all project `Automaat/sybra`) and read
each run's full NDJSON log under `~/.sybra/logs/agents/`, plus the matching
`agent.completed` audit event, to bucket the failures by root cause.

## Buckets

| Bucket | Count | Share |
|---|---|---|
| Killed by workflow watchdog before producing a verdict | 39 | 95% |
| — of which: genuine environment/tooling flakiness self-reported via `unable_to_run_reason` | 2 | 5% |
| — of which: died immediately after `init` with zero further output | 1 | 2% |
| Reached a valid verdict, misclassified as `failed` in stats | 1 | 2% |
| Genuine test-runner/product-competence failure | 0 | 0% |

**None of the 41 sampled failures are the test-runner failing to do its
job correctly.** The 22% headline failure rate is overwhelmingly an
artifact of the workflow watchdog killing and re-dispatching test-runner
agents before they finish, plus a smaller stats-classification bug.

## Watchdog kill-before-verdict (39/41)

All 39 runs show `costUsd: 0` in `stats.json` despite real work (10-89
turns, 16-357s wall-clock) — consistent with the process being killed
before it ever emits its own terminal cost/result summary, rather than
finishing and failing on its own.

Retries cluster tightly on a handful of tasks instead of being spread
evenly across the window:

- Task `2d23a2a5` — 7 consecutive test-runner attempts between 07:06:44
  and 07:27:33 (~every 2-5 min), all `costUsd: 0`, before an 8th attempt
  produced a verdict and let the task advance to `ready-pr`.
- Task `4eb2fa30` — 5 attempts.
- Task `e000da0e` — 5 attempts.
- Task `699d687b` — 3 attempts.
- Task `e74905de` — 3 attempts (one, `aa2dceeb`, died right after `init`
  with no further output at all).

This is a named failure mode in the codebase already: `clearAgentStepsForTask`
in `internal/workflow/engine_events.go` documents that "a stopped
test-runner's late provider-error completion lands on the still-current
run_test step and burns the retry budget before the retry agent has
produced a verdict." The watchdog hang-retry / rate-limit-reschedule
machinery (`handleWatchdogHangRetry`, `RescheduleRateLimitedAgent`) is what
kills and re-dispatches these runs — test-runner's job (drive a real
server surface, run adversarial probes, restore the worktree) is slower and
burstier than other roles, and the hang-detection window appears tuned for
faster roles.

Filed as [#1664](https://github.com/Automaat/sybra/issues/1664).

Two of the 39 are legitimate environment flakiness, honestly self-reported
by the agent's own protocol (`unable_to_run_reason`), not watchdog kills:

- `983480b3` — the manual-test port file disappeared between the initial
  health probe and the follow-up API probe.
- `cfe38589` — codex's own API client hit `Connection refused` on its
  responses websocket mid-run.

## Verdict-reached-but-misclassified (1/41)

`728114f2` (codex, task `929fb7f0`) reached a clean, confident `FAIL`
verdict as its last emitted message — a real product bug (a shared
`viewer` GraphQL error silently ignored in batched PR fetches), with a full
reproduction command, verbatim test output, and an empty
`unable_to_run_reason` — yet is still recorded `outcome: failed` in
`stats.json`.

`runOutcome` (`internal/sybra/completion/completion.go`) is explicitly designed
to rescue exactly this case for test-runner, but the rescue isn't landing
for this run. Filed as
[#1665](https://github.com/Automaat/sybra/issues/1665).

## Non-finding

No sampled run showed the test-runner reaching a verdict, having ample
time/budget to do so, and simply getting it wrong. Tuning the test-runner
prompt or gates would not have moved the failure rate here — the fix has
to land in the watchdog/retry layer (#1664) and the stats classification
path (#1665).
