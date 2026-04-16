# Sybra perf-testing guide

Everything is token-free — fake-claude replaces the real `claude` CLI via PATH injection in an isolated `SYBRA_HOME` tempdir. Your real `~/.sybra/` is never touched.

## 1. Go benchmarks (no server needed)

Fast, repeatable microbenchmarks on the four perf-critical packages.

```bash
mise run perf:bench
# or directly:
go test -run XXX -bench . -benchmem -benchtime=2s ./internal/task/ ./internal/agent/ ./internal/sse/ ./internal/watcher/
```

Knobs:

- `-benchtime=5s` — longer runs reduce variance; `-benchtime=100x` for fixed iteration counts.
- `-count=3` — repeats each benchmark 3× (feed to `benchstat` for A/B comparisons).
- `-cpuprofile=cpu.out -memprofile=mem.out` — emit pprof profiles (analyze with `go tool pprof cpu.out`).

Baseline reference (M4 Max, `-benchtime=2s`):

- `BenchmarkStoreList/n=1000` ≈ 39 ms/op — UI refresh hot path budget.
- `BenchmarkParseClaudeLine_Mixed` ≈ 2.4 µs/op — every NDJSON line pays this.
- `BenchmarkEmit_AllSubscribers/subs=100` ≈ 18 µs/op — SSE fanout cost.
- `BenchmarkWatcherSameFileCoalescing` — verifies N writes → 1 emission.

Compare two runs:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -run XXX -bench . -benchmem -count=6 ./internal/task/ > before.txt
# ...make changes...
go test -run XXX -bench . -benchmem -count=6 ./internal/task/ > after.txt
benchstat before.txt after.txt
```

## 2. End-to-end harness (`cmd/sybra-perf`)

Build the harness + fake-claude once:

```bash
mise run perf:build
# produces bin/sybra-perf and bin/fake-claude
# sybra-server is spawned via `go run` inside the harness (cached on rebuild)
```

### Scenarios

**steady** — N agents ramped over T seconds, held for `-duration`. Each agent runs `perf_stream` (N events at M ms interval). Primary measurement: agent startup latency.

```bash
./bin/sybra-perf -scenario steady \
  -concurrency 8 -duration 30s -ramp 2s \
  -events 200 -event-interval 10ms \
  -fake-claude ./bin/fake-claude
```

**spike** — No ramp. All agents start as fast as possible with `perf_burst` (zero-interval events). Stresses the 50 ms emit throttle and concurrency bookkeeping.

```bash
./bin/sybra-perf -scenario spike \
  -concurrency 16 -duration 10s \
  -events 500 \
  -fake-claude ./bin/fake-claude
```

**soak** — Long runtime with `perf_long`. Pair with `-pprof-dir` for before/after heap diffs — the point is catching goroutine or buffer leaks.

```bash
./bin/sybra-perf -scenario soak \
  -concurrency 4 -duration 5m \
  -event-interval 200ms \
  -fake-claude ./bin/fake-claude \
  -pprof-dir ./perf-profiles
```

**churn** — No agents. Hammers `TaskService.CreateTask / UpdateTask / DeleteTask` at a target rate. Updates touch `body` only (no hooks fire), so the numbers reflect pure CRUD latency.

```bash
./bin/sybra-perf -scenario churn \
  -concurrency 4 -duration 30s -churn-rate 200 \
  -fake-claude ./bin/fake-claude
```

### Pre-built mise shortcuts

```bash
mise run perf:load    # steady, 8 agents, 30s
mise run perf:churn   # 200 ops/sec, 30s
mise run perf:soak    # 5 min soak + pprof
```

### Flag reference

| Flag              | Default                   | Notes                                                              |
| ----------------- | ------------------------- | ------------------------------------------------------------------ |
| `-scenario`       | `steady`                  | `steady` \| `spike` \| `soak` \| `churn`                           |
| `-concurrency`    | 4                         | agents (or churn workers)                                          |
| `-duration`       | 30s                       | total runtime                                                      |
| `-ramp`           | 2s                        | steady/soak ramp-up window                                         |
| `-events`         | 100                       | fake-claude event count (stream/burst)                             |
| `-event-interval` | 10ms                      | fake-claude inter-event delay                                      |
| `-churn-rate`     | 100                       | ops/sec for churn                                                  |
| `-fake-claude`    | *(builds via `go run`)*   | pre-built binary path (faster)                                     |
| `-workdir`        | *(tmp)*                   | custom SYBRA_HOME — use to inspect server state after a run      |
| `-pprof-dir`      | *(off)*                   | heap + goroutine snapshots before/after                            |
| `-report`         | *(stdout)*                | write JSON report to a file                                        |
| `-keep-home`      | false                     | do not delete the temp SYBRA_HOME                                |
| `-verbose`        | false                     | log server output and per-agent progress                           |

### Saving reports cleanly

The JSON report goes to stdout by default. Piping it through `grep`/`head` will truncate. Use `-report`:

```bash
./bin/sybra-perf -scenario churn -duration 30s -churn-rate 200 \
  -fake-claude ./bin/fake-claude \
  -report /tmp/churn.json

jq '.createLatencyMS, .updateLatencyMS' /tmp/churn.json
```

## 3. Interpreting the report

Top-level keys:

- `startupLatencyMS` (steady/spike/soak) — `count / min / p50 / p95 / p99 / max / mean` derived from per-agent StartAgent HTTP latency.
- `totalEvents` — every SSE event received during the scenario.
- `outputEvents` — agent-level throughput (sum across all agents).
- `agents[]` — per-agent `agentId`, `taskId`, `startLatencyMS`, `observedEventCount`, `error`.
- `createLatencyMS / updateLatencyMS / deleteLatencyMS` (churn) — `count / min / p50 / p95 / p99 / max / mean` for each operation.
- `metricsBefore / metricsAfter` — Prometheus snapshot of `sybra_agent_*`, `sybra_task_*`, `go_goroutines`, `process_resident_memory_bytes`. Subtract to see the scenario's impact.

## 4. pprof workflow

Pull profiles during or after a soak/steady run:

```bash
./bin/sybra-perf -scenario soak -duration 5m -concurrency 4 \
  -fake-claude ./bin/fake-claude -pprof-dir ./perf-profiles

# files produced: heap-before.pb.gz, heap-after.pb.gz,
#                 goroutine-before.pb.gz, goroutine-after.pb.gz
go tool pprof -http=:7070 ./perf-profiles/heap-after.pb.gz
go tool pprof -diff_base ./perf-profiles/heap-before.pb.gz ./perf-profiles/heap-after.pb.gz
```

For on-demand profiles against a live harness run, start the server manually with pprof:

```bash
SYBRA_PPROF=1 SYBRA_HOME=/tmp/sybra-manual go run ./cmd/sybra-server
# in another shell:
curl -o cpu.pb.gz "http://127.0.0.1:8080/debug/pprof/profile?seconds=30"
curl -o goroutines.txt "http://127.0.0.1:8080/debug/pprof/goroutine?debug=2"
```

## 5. Troubleshooting

- **First run is slow** — `go run ./cmd/sybra-server` pays a cold compile cost (~15–25s). Subsequent runs are cached by Go's build cache.
- **"Dir not accessible"** — the harness creates `research/` inside the temp home; if this surfaces, your `-workdir` is on a filesystem the server can't stat. Use the default temp path.
- **Steady report shows `observedEventCount: 0`** — agents started but didn't stream events back before the scenario ended. Increase `-duration` or drop `-event-interval`.
- **Churn returns `churnErrors > 0`** — check server logs with `-verbose`; usually the churn worker pool is saturated (raise `-concurrency`).
- **pprof files are empty** — `SYBRA_PPROF=1` must be set on the server; the harness sets it automatically when you pass `-pprof-dir`.
- **Want to peek at server state after a run** — pass `-keep-home -workdir /tmp/sybra-debug` and the whole SYBRA_HOME (tasks, logs, audit) stays on disk for inspection.

## 6. Files created for perf work

- `cmd/fake-claude/main.go` — new `perf_stream`/`perf_burst`/`perf_long` scenarios (+tests in `main_test.go`)
- `cmd/sybra-perf/main.go` — the e2e harness binary
- `cmd/sybra-server/main.go` — `/debug/pprof/*` mounted behind `SYBRA_PPROF=1`
- `internal/task/store_bench_test.go`
- `internal/agent/stream_bench_test.go`
- `internal/sse/broker_bench_test.go`
- `internal/watcher/watcher_bench_test.go`
- `mise.toml` — `perf:bench`, `perf:build`, `perf:load`, `perf:churn`, `perf:soak` tasks
