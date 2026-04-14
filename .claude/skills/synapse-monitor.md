---
name: synapse-monitor
description: Deprecated. Monitor now runs in-process inside Synapse. Use `synapse-cli monitor scan` for an ad-hoc detector pass or read the `monitor:report` event from the GUI.
allowed-tools: Bash, Read
user-invocable: true
---

# Synapse Monitor (legacy skill stub)

The monitor loop previously driven by `/loop 5m /synapse-monitor` now runs
inside the Synapse Go backend (`internal/monitor/`). It ticks every 5 min,
detects anomalies, applies idempotent remediations, and spawns focused
headless Claude agents for anything that needs LLM judgment.

This skill no longer runs in a loop. Do **not** register a cron or `/loop`
for it.

## Read-only ad-hoc pass

```bash
synapse-cli monitor scan            # human summary line
synapse-cli monitor scan --json     # full Report JSON
```

The `--json` output matches `monitor.Report` in `internal/monitor/report.go`
and is emitted over the Wails `monitor:report` event on every tick.

## What moved to Go

- **Board + audit snapshot, dwell check, threshold detection** — `internal/monitor/detector.go`.
- **`lost_agent` reset + `untriaged` status reason** — `internal/monitor/remediator.go`.
- **Issue dedup + creation via `gh`** — `internal/monitor/issuesink.go`.
- **LLM dispatch (pr_gap, stuck_human_blocked, failure_spike, bottleneck)** —
  `internal/monitor/dispatcher.go` via `agent.Manager.Run`.
- **Heartbeat** — removed. The in-process service is its own liveness signal.

If something in the loop is broken, start at `internal/monitor/service.go`
and follow the references.
