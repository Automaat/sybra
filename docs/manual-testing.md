# Manual Testing Sybra

Use this guide when automated tests pass but a feature needs to be exercised through the real Sybra runtime: config loading, HTTP/Wails-bound services, workflow dispatch, agent runners, task files, stats, audit, and evaluation reports.

## Rules

- Use an isolated `SYBRA_HOME` for every manual run. Do not write test tasks, stats, logs, projects, or workflows into your real `~/.sybra`.
- Use only public/pet repositories for project/worktree tests. Do not use work-typed repos or paste work-derived content into test artifacts.
- Disable unrelated automations unless they are the feature under test. In particular, disable monitor, GitHub fetcher, Todoist, Renovate, umbrella expansion, and orchestrator auto-plan/auto-triage.
- Prefer fake local provider CLIs for routing/accounting tests. Use real Claude/Codex/Copilot only when the behavior being tested requires the real provider.
- Bind servers to an ephemeral port and clean up processes and temp directories afterward.

## Isolated server smoke harness

This starts the real HTTP server with isolated data and fake provider CLIs. It exercises the same service registry and workflow/agent paths as the web UI without spending model credits.

```bash
TMP=$(mktemp -d -t sybra-smoke-XXXXXX)
PORT=$(python3 - <<'PY'
import socket
s=socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)

mkdir -p "$TMP/home/tasks" "$TMP/home/logs" "$TMP/home/workflows" "$TMP/fakebin"
```

Create an isolated config:

```bash
cat > "$TMP/home/config.yaml" <<YAML
logging:
  level: info
  dir: $TMP/home/logs
  max_size_mb: 10
  max_files: 2
audit:
  enabled: true
  retention_days: 7
agent:
  provider: claude
  max_concurrent: 20
  max_cost_usd: 5
  max_turns: 50
  require_permissions: false
orchestrator:
  auto_triage: false
  auto_plan: false
  dispatch_interval_seconds: 3600
  maintenance_interval_seconds: 3600
providers:
  health_check: { enabled: false }
  auto_failover: false
  limits: { enabled: false }
  claude: { enabled: true }
  codex: { enabled: true }
  copilot: { enabled: true }
monitor: { enabled: false }
github: { enabled: false }
renovate: { enabled: false }
todoist: { enabled: false }
umbrella: { enabled: false }
triage: { enabled: false }
evaluation:
  enabled: true
  interval_hours: 24
  window_days: 30
YAML
```

Create fake CLIs:

```bash
cat > "$TMP/fakebin/claude" <<'SH'
#!/usr/bin/env bash
printf '{"type":"system","session_id":"fake-claude-session"}\n'
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"claude ok"}]}}\n'
printf '{"type":"result","subtype":"success","session_id":"fake-claude-session","result":"claude done","total_cost_usd":0.0123,"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}\n'
SH

cat > "$TMP/fakebin/codex" <<'SH'
#!/usr/bin/env bash
printf '{"type":"thread.started","thread_id":"fake-codex-session"}\n'
printf '{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"codex ok"}}\n'
printf '{"type":"turn.completed","usage":{"input_tokens":12,"cached_input_tokens":2,"output_tokens":6,"reasoning_output_tokens":1}}\n'
SH

cat > "$TMP/fakebin/copilot" <<'SH'
#!/usr/bin/env bash
printf '{"type":"assistant.message","data":{"messageId":"m1","model":"gpt-5.5","content":"copilot ok","toolRequests":[],"outputTokens":4}}\n'
printf '{"type":"result","sessionId":"fake-copilot-session","exitCode":0,"usage":{"premiumRequests":7.5,"sessionDurationMs":100,"totalApiDurationMs":50}}\n'
SH

chmod +x "$TMP/fakebin/claude" "$TMP/fakebin/codex" "$TMP/fakebin/copilot"
```

Start the server:

```bash
PATH="$TMP/fakebin:$PATH" \
SYBRA_HOME="$TMP/home" \
SYBRA_PORT="$PORT" \
go run ./cmd/sybra-server
```

In another shell:

```bash
curl -fsS "http://127.0.0.1:$PORT/health"
```

## HTTP API calls

The server exposes allowlisted bound methods as:

```text
POST /api/{Service}/{Method}
```

Request bodies are JSON arrays of positional arguments.

Examples:

```bash
api() {
  curl -fsS -H 'Content-Type: application/json' \
    -X POST "http://127.0.0.1:$PORT/api/$1/$2" \
    --data "$3"
}

api App GetEvaluationReport '[]' | jq .
api TaskService ListTasks '[]' | jq .
```

## Avoiding accidental default workflows

`TaskService.CreateTask` auto-starts the normal `task.created` workflow. For a custom manual workflow, create the task through `sybra-cli` with a tag that `skipTaskCreatedWorkflow` ignores:

```bash
SYBRA_HOME="$TMP/home" go run ./cmd/sybra-cli --json create \
  --title "AB smoke one" \
  --body "exercise A/B assignment" \
  --mode headless \
  --tags handoff-manual | tee "$TMP/task.json"

TASK_ID=$(jq -r .id "$TMP/task.json")
export TASK_ID
```

Then start only the workflow under test:

```bash
api WorkflowService StartWorkflow "[\"$TASK_ID\",\"ab-smoke\"]"
```

## A/B assignment smoke workflow

Use a no-worktree author role so A/B assignment applies without needing a project clone:

```bash
cat > "$TMP/home/workflows/ab-smoke.yaml" <<'YAML'
id: ab-smoke
name: AB Smoke
trigger:
  on: manual
steps:
  - id: run_variant
    name: Run Variant
    type: run_agent
    config:
      role: fix-review
      mode: headless
      model: sonnet
      prompt: "AB smoke: emit a clean result."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
YAML
```

Acceptance checks:

```bash
api TaskService GetTask "[\"$TASK_ID\"]" | jq '.status, .agentRuns[-1]'
cat "$TMP/home/stats.json" | jq '.[-1]'
api App GetEvaluationReport '[]' | jq '.byAgentModel, .byExperimentKind'
```

Expected:

- `agentRuns[-1]` has `model`, `experimentId`, `variantId`, `assignmentUnit`, and `assignmentKey`.
- `stats.json[-1]` has the same A/B metadata.
- Copilot smoke runs preserve fractional `premiumRequests` such as `7.5`.
- Evaluation report includes rows in `byAgentModel` and `byExperimentKind` groups (`kind: "model"`, `"prompt"`, `"skill"`, `"unknown"`), each with a `groups[]` list keyed by `experimentId` (never mixing two experiments' rows), and each `groups[].rows`/`groups[].rowsContribution` breakdown carrying role drilldowns when multiple roles are present.

To test landed-task attribution in Evaluation, append a synthetic audit event in the isolated audit log after the workflow finishes:

```bash
mkdir -p "$TMP/home/logs/audit"
DAY=$(date -u +%F)
python3 - <<PY >> "$TMP/home/logs/audit/$DAY.ndjson"
import json, datetime, os
print(json.dumps({
  "ts": datetime.datetime.now(datetime.UTC).isoformat().replace("+00:00", "Z"),
  "type": "task.landed",
  "task_id": os.environ["TASK_ID"],
  "data": {"outcome": "merged", "created_to_land_h": 0.01, "work_to_land_h": 0.01},
}))
PY
```

Then refresh:

```bash
api App GetEvaluationReport '[]' | jq '.byExperimentKind'
```

## Testing with real providers

Use the same isolated server setup, but omit `"$TMP/fakebin"` from `PATH`. For Copilot model smoke tests, use no-op prompts first:

```bash
copilot -p 'Respond with exactly: OK' --model claude-opus-4.6 \
  --output-format json --allow-all-tools --no-ask-user --no-custom-instructions --log-level error

copilot -p 'Respond with exactly: OK' --model gpt-5.5 \
  --output-format json --allow-all-tools --no-ask-user --no-custom-instructions --log-level error

copilot -p 'Respond with exactly: OK' --model gemini-3.1-pro-preview \
  --output-format json --allow-all-tools --no-ask-user --no-custom-instructions --log-level error
```

Check the final `result.usage.premiumRequests` and `assistant.message.data.model`.

## Testing with a project/worktree

Use public/pet projects only. The dedicated manual-testing repository is
`Automaat/sybra-testbed` (`https://github.com/Automaat/sybra-testbed`), a tiny
Express app intended for Sybra adversarial testing. Do not use work-typed
projects for public Sybra manual-test artifacts.

If it is not registered on the machine yet, add it in the isolated home:

```bash
GIT_CONFIG_COUNT=1 \
GIT_CONFIG_KEY_0=safe.bareRepository \
GIT_CONFIG_VALUE_0=all \
SYBRA_HOME="$TMP/home" go run ./cmd/sybra-cli --json project create \
  --url https://github.com/Automaat/sybra-testbed \
  --type pet
```

The `GIT_CONFIG_*` override keeps the smoke isolated on machines whose global
Git config sets `safe.bareRepository=explicit`; Sybra's project manager operates
on bare clones under `SYBRA_HOME/clones`.

For a real worktree smoke:

1. Create the task with `--tags handoff-manual`.
2. Set `project_id` to `Automaat/sybra-testbed`.
3. Start the target workflow explicitly.
4. Inspect task run history, worktree path, stats, and audit logs.

`Automaat/sybra-testbed` is also usually cloned at
`~/sideprojects/sybra-testbed`, which is useful when you need to inspect the app
or create a local bare clone without network access.

## Cleanup

Stop the server and remove the temp home:

```bash
rm -rf "$TMP"
```

If a server was started through an agent shell session, stop that session with the matching shell id rather than killing by process name.
