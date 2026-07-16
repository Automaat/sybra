# Server error-rate SLO and alerting

This documents the reliability target for `sybra-server`'s HTTP surface and
how Sybra detects and alerts on breaches. It complements the existing
agent-run failure-rate SLO (`monitor.failure_rate_threshold`) — that one
measures whether *dispatched agents* succeed; this one measures whether the
*HTTP API itself* is healthy, independent of any agent activity.

## The target

**5xx response rate < 1%, evaluated over a rolling 15-minute window, once
that window has seen at least 20 requests.**

The volume gate matters: on a quiet deployment a single failed request out of
three total requests is a 33% error rate but not a signal worth escalating.
The gate defers evaluation until there's enough traffic for the rate to mean
something.

| Config key | Default | Meaning |
|---|---|---|
| `monitor.http_error_rate_threshold` | `0.01` | 5xx / total ratio that trips the alert |
| `monitor.http_error_rate_window_minutes` | `15` | Rolling window width |
| `monitor.http_error_rate_min_requests` | `20` | Minimum requests in the window before the threshold is evaluated |

Override any of these in `~/.sybra/config.yaml` under `monitor:` (see
[`docs/CONFIG.md`](CONFIG.md#monitorconfig-monitor) for the full field
reference); `sybra-cli config dump` shows the resolved values.

## How it's measured

Every HTTP response `sybra-server` serves (except `GET /health`, a
high-frequency liveness probe that isn't real API traffic) is recorded by
`metricsMiddleware` (`cmd/sybra-server/main.go`) into two places:

1. **`internal/httpstats.Tracker`** — an in-process, one-minute-bucketed
   sliding window kept inside `internal/metrics` (`httpTracker`). This is
   what the SLO detector reads, and it works whether or not Prometheus
   export is enabled — the error-rate SLO must be enforceable even on a
   deployment that never turns on `/metrics`.
2. **`sybra_http_requests_total{class="2xx"|"3xx"|"4xx"|"5xx"}`** — a
   Prometheus counter, exported at `GET /metrics` when `metrics.enabled` is
   set. Use this for external dashboards/alerting (Grafana, Alertmanager,
   etc.) alongside the other `sybra_*` instruments in `internal/metrics`.

## What happens on a breach

Every monitor tick (`monitor.interval_seconds`, default 5 minutes),
`internal/monitor`'s `detectHTTPErrorRate` reads a fresh
`metrics.HTTPErrorSnapshot` for the configured window. When the rate exceeds
the threshold with enough volume, it raises a `KindHTTPErrorSpike` anomaly
(severity `error`), which the monitor `Service` dispatches to a focused
headless investigation agent (`httpErrorSpikePrompt` in
`internal/monitor/prompts.go`). That agent greps the server logs
(`journalctl -u sybra` on the deployed host, or
`~/.sybra/logs/sybra.log` locally) for the failure window, checks whether
failures cluster on one API method or a recent deploy, and files/updates a
GitHub issue on `monitor.issue_repo` labeled `monitor` (title
`[monitor] http_error_spike`) — the same pipeline `failure_spike` and
`bottleneck` anomalies already use. See the "Monitor loop" and "Escalation
Rules" sections of the root `CLAUDE.md` for how anomalies fit into the wider
triage/dispatch loop.

## Verifying it manually

The HTTP error-rate window lives in the `sybra-server` process's memory
(`internal/metrics.httpTracker`), not on disk — unlike the other monitor
signals (task/audit-derived), it is **not** visible to a separate
`sybra-cli monitor scan` invocation, since that spawns its own short-lived
process with an empty window. Check it against the live server instead:

```bash
# Resolved thresholds
sybra-cli config dump | grep -A3 http_error_rate

# Prometheus counters, if metrics.enabled — reads the live server's window
curl -s http://localhost:8080/metrics | grep sybra_http_requests_total

# Confirm the in-process detector ran and see anomaly counts for this tick
ssh root@192.168.20.219 "journalctl -u sybra -n 50 --no-pager | grep monitor.tick"
```
