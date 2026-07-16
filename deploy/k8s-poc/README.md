# Sybra Kubernetes agent runner PoC

Runs `sybra-server` as a Kubernetes Deployment and routes headless agents to
short-lived Kubernetes Jobs. This is a smoke-test harness, not a production
deployment.

## Run locally with k3d

```bash
docker build -t sybra-server:poc .
k3d cluster create sybra-poc --agents 1 --wait
k3d image import sybra-server:poc -c sybra-poc
kubectl apply -k deploy/k8s-poc
kubectl -n sybra-poc rollout status deployment/sybra-server --timeout=120s
```

Start a fake headless agent Job through Sybra:

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/App/StartK8sPocAgent' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data '["prove k8s job spawning"]'
```

Inspect the Job and Sybra's parsed agent output:

```bash
kubectl -n sybra-poc get jobs,pods -l app.kubernetes.io/name=sybra-agent
kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/AgentService/ListAgents' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data '[]'
```

Clean up:

```bash
k3d cluster delete sybra-poc
```

The default agent Job uses `busybox:1.36` and emits Claude-shaped NDJSON so the
existing Sybra stream parser can consume it.

## Instance role

A Kubernetes deployment should not start orchestrator sessions or dispatch tasks
just because tasks are active — a shared or test cluster would race the machine
that actually owns the board. Every config here sets:

```yaml
orchestrator:
  role: agent-only
```

`agent-only` fails closed on both self-starting automations:

| | `full` (default) | `agent-only` |
|---|---|---|
| Orchestrator brain session (auto-start) | yes | no |
| Workflows of any kind — dispatch, resume, status-change, **or a manual start** | yes | no |
| Board reconcile, stale-agent restart | yes | no |
| Auto-spawned human-review agent | yes | no |
| HTTP API, explicitly-started agents (`App.StartAgent`, `sybra-cli`) | yes | yes |
| Draining an explicitly-started agent queued behind a busy pool | yes | yes |
| Maintenance cleanup (orphan worktrees/sandboxes, metrics) | yes | yes |

**An `agent-only` instance runs agents, not workflows.** The workflow engine is
the single gate: `StartWorkflow`, `DispatchEvent`, `HandleStatusChange` and
`ResumeStalled` all refuse with `workflow dispatch is disabled on this instance`.
That is deliberately blunt — it stops an operator-initiated workflow start too,
not just automatic ones — because the engine has callers spread across the task
service, the review fixer, completion, PR integrations and the watcher, and
gating those individually kept missing routes. Direct agent starts never touch
the engine, so they keep working. Opt back in with `scheduler_enabled: true`.

Two things deliberately keep running under `agent-only`. Cleanup, so a parked
instance still collects its own garbage. And the manual queue drain — that is the
resume path for an agent an operator already explicitly started, not
auto-dispatch, so gating it would strand any start that landed while the pool was
busy. An operator's manual `StartOrchestrator` call is never gated either.

Re-enable either half independently — these win over `role`:

```yaml
orchestrator:
  role: agent-only
  enabled: true            # orchestrator brain only
  scheduler_enabled: true  # auto-dispatch only
```

An invalid `role` falls back to `full` and logs `config.orchestrator.role.invalid`,
so a typo never silently parks an instance that was meant to orchestrate. Note the
direction: a typo'd role starts orchestrating, it does not go idle — check the log
after changing it.

The role is sampled once at startup (like `orchestrator.dispatch_interval_seconds`),
so changing it needs a restart; a reload logs `config.reload.restart_required`
with field `orchestrator.role`.

Confirm the resolved role in the startup log. Note `kubectl logs` will *not* show
it: Sybra's slog handler writes to a rotating file, not stdout, so the pod's
stdout carries only a few bootstrap lines. Read the real sink instead:

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- \
  grep app.automations /home/sybra/.sybra/logs/sybra.log
```

```json
{"level":"INFO","msg":"app.automations","instance_role":"agent-only","orchestrator":false,"scheduler":false}
```

## Fake repo e2e mode

Use this path when you want a fully local k3d test that exercises the real
Sybra project/worktree path without model API keys or GitHub pushes. The server
and agent Job share the `sybra-home` PVC, the Job runs `/usr/local/bin/fake-claude`,
and the fake provider writes `k8s-agent-output.txt` into the checked-out repo.
The Kubernetes wrapper commits and pushes the change back to the local bare repo,
then Sybra fast-forwards the server-side worktree.

Apply the fake-repo config and restart the server:

```bash
kubectl apply -f deploy/k8s-poc/config-fake-repo.yaml
kubectl -n sybra-poc rollout restart deployment/sybra-server
kubectl -n sybra-poc rollout status deployment/sybra-server --timeout=120s
```

Seed a disposable GitHub-shaped project backed by a local bare repo on the PVC:

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- sh -ceu '
HOME_DIR=/home/sybra/.sybra
OWNER=FakeOrg
REPO=k8s-testbed
SRC=/tmp/k8s-testbed-src
BARE="$HOME_DIR/clones/$OWNER/$REPO.git"

rm -rf "$SRC" "$BARE" "$HOME_DIR/worktrees/k8s-smoke-"*
mkdir -p "$HOME_DIR/clones/$OWNER" "$HOME_DIR/projects"
git init -b main "$SRC"
git -C "$SRC" config user.email fake-repo@example.invalid
git -C "$SRC" config user.name "Fake Repo"
printf "hello from fake repo\n" > "$SRC/README.md"
git -C "$SRC" add README.md
git -C "$SRC" commit -m "chore: seed fake repo"
git clone --bare "$SRC" "$BARE"
git --git-dir="$BARE" config remote.origin.url "$BARE"
git --git-dir="$BARE" config remote.origin.fetch "+refs/heads/*:refs/remotes/origin/*"
git --git-dir="$BARE" update-ref refs/remotes/origin/main "$(git --git-dir="$BARE" rev-parse refs/heads/main)"
cat > "$HOME_DIR/projects/$OWNER--$REPO.yaml" <<YAML
id: $OWNER/$REPO
name: $REPO
owner: $OWNER
repo: $REPO
url: https://github.com/$OWNER/$REPO.git
clone_path: $BARE
type: pet
status: ready
worktree_base_ref: fresh
created_at: "2026-07-14T00:00:00Z"
updated_at: "2026-07-14T00:00:00Z"
YAML
'
```

Create a manual task assigned to that project:

```bash
TASK_ID=$(
  kubectl -n sybra-poc exec deploy/sybra-server -- \
    sybra-cli --json create \
      --title "k8s fake repo smoke" \
      --body "Run fake Claude in a Kubernetes Job and write a repo marker file." \
      --mode headless \
      --project FakeOrg/k8s-testbed \
      --tags handoff-manual,k8s-e2e \
      --allow-dup \
  | jq -r .id
)
```

Start the agent through Sybra. This is intentionally `App.StartAgent`, not the
normal task-created workflow, so the smoke stays deterministic and only proves
the project/worktree/agent backend:

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/App/StartAgent' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data "[\"$TASK_ID\",\"headless\",\"Write the Kubernetes fake repo marker file.\",true]" \
  | jq .
```

Wait for the Job and verify that Sybra parsed the output and fast-forwarded the
server worktree:

```bash
kubectl -n sybra-poc wait --for=condition=complete job -l app.kubernetes.io/name=sybra-agent --timeout=180s

kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/AgentService/ListAgents' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data '[]' \
  | jq '.[] | select(.taskId == "'$TASK_ID'") | {id, taskId, state, command, provider, model, costUsd}'

kubectl -n sybra-poc exec deploy/sybra-server -- \
  sybra-cli --json get "$TASK_ID" \
  | jq -r .worktreeDir

WT=$(
  kubectl -n sybra-poc exec deploy/sybra-server -- \
    sybra-cli --json get "$TASK_ID" \
  | jq -r .worktreeDir
)
kubectl -n sybra-poc exec deploy/sybra-server -- \
  sh -ceu "test -f '$WT/k8s-agent-output.txt' && cat '$WT/k8s-agent-output.txt' && git -C '$WT' log --oneline -2"
```

Expected output includes:

- an agent whose command is `kubernetes job/sybra-agent-...`
- `state: stopped`
- `provider: claude`
- `k8s-agent-output.txt` containing `changed by k8s fake agent`
- a git commit titled `chore: persist k8s agent changes`

## API-key provider mode

The runner can also start the selected provider CLI in the Job and inject API
keys from a Kubernetes Secret. The provider-mode example uses OpenCode with
`openrouter/deepseek/deepseek-v4-flash` so e2e tests can run against a cheap
OpenRouter-backed model:

```bash
cp deploy/k8s-poc/.env.example deploy/k8s-poc/.env
$EDITOR deploy/k8s-poc/.env
set -a
. deploy/k8s-poc/.env
set +a

kubectl -n sybra-poc create secret generic sybra-provider-api-keys \
  --from-literal=openrouter_api_key="$OPENROUTER_API_KEY" \
  --from-literal=github_token="$GITHUB_TOKEN"
```

Then replace the default fake ConfigMap with the provider-mode example:

```bash
kubectl apply -f deploy/k8s-poc/config-provider-example.yaml
kubectl -n sybra-poc rollout restart deployment/sybra-server
kubectl -n sybra-poc rollout status deployment/sybra-server --timeout=120s
```

Provider mode expects the agent image to contain the provider CLI. The example
uses `sybra-server:poc`, which includes `claude`, `codex`, and `opencode`.

When Sybra dispatches a normal task whose local worktree has an `origin` remote,
the server first pushes the current task branch, then the Job clones that remote
into `/workspace/repo`, checks out the task branch, runs the provider CLI there,
commits any remaining dirty changes, and pushes the branch back to `origin`.
After the Job completes, the local task worktree fast-forwards from `origin` so
review/test stages see the pod's files. The `GITHUB_TOKEN` value is optional for
public HTTPS remotes, but required for private GitHub HTTPS remotes and pushes.
SSH remotes are not wired in this PoC.

## Real OpenCode testbed e2e

This is the end-to-end path used to prove the PoC with a real model. It uses
`Automaat/sybra-testbed` and OpenCode via OpenRouter. Example issue:
`https://github.com/Automaat/sybra-testbed/issues/9`.

Apply provider mode first, then register the testbed project:

```bash
kubectl apply -f deploy/k8s-poc/config-provider-example.yaml
kubectl -n sybra-poc rollout restart deployment/sybra-server
kubectl -n sybra-poc rollout status deployment/sybra-server --timeout=120s

kubectl -n sybra-poc exec deploy/sybra-server -- \
  sybra-cli project create https://github.com/Automaat/sybra-testbed.git
```

For a fully disposable k3d run that does not push branches to GitHub, point the
PVC clone's `origin` back at the local bare clone:

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- sh -ceu '
BARE=/home/sybra/.sybra/clones/Automaat/sybra-testbed.git
git --git-dir="$BARE" config remote.origin.url "$BARE"
git --git-dir="$BARE" config remote.origin.fetch "+refs/heads/*:refs/remotes/origin/*"
git --git-dir="$BARE" config receive.denyCurrentBranch updateInstead
git --git-dir="$BARE" update-ref refs/remotes/origin/main "$(git --git-dir="$BARE" rev-parse refs/heads/main)"
'
```

Create a task against the testbed issue and start a headless OpenCode agent:

```bash
TASK_ID=$(
  kubectl -n sybra-poc exec deploy/sybra-server -- \
    sybra-cli --json create \
      --title "k8s e2e: add /version endpoint" \
      --body "Implement https://github.com/Automaat/sybra-testbed/issues/9. Add GET /version to server.js. It must return status 200, text/plain, and exactly the package.json version string 1.0.0. Keep / and /healthz unchanged." \
      --mode headless \
      --project Automaat/sybra-testbed \
      --tags handoff-manual,k8s-e2e,opencode \
      --allow-dup \
  | jq -r .id
)

kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/App/StartAgent' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data "[\"$TASK_ID\",\"headless\",\"Implement issue #9 in the testbed repo. Add GET /version returning the package.json version as text/plain. Keep / and /healthz unchanged. Do not create a PR.\",true]" \
  | jq .
```

Wait for the Job, then inspect Sybra's parsed run metadata:

```bash
kubectl -n sybra-poc wait --for=condition=complete job -l app.kubernetes.io/name=sybra-agent --timeout=240s

kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/AgentService/ListAgents' \
    -H 'Authorization: Bearer poc-token' \
    -H 'Content-Type: application/json' \
    --data '[]' \
  | jq '.[] | select(.taskId == "'$TASK_ID'") | {id, taskId, state, command, provider, model, costUsd, inputTokens, outputTokens}'
```

Verify the branch commit and runtime behavior in the server worktree:

```bash
WT=$(
  kubectl -n sybra-poc exec deploy/sybra-server -- \
    sybra-cli --json get "$TASK_ID" \
  | jq -r .worktreeDir
)

kubectl -n sybra-poc exec deploy/sybra-server -- \
  sh -ceu "git -C '$WT' status --short && git -C '$WT' show --stat --oneline HEAD && git -C '$WT' show --name-only --oneline HEAD"

kubectl -n sybra-poc exec deploy/sybra-server -- sh -ceu "
cd '$WT'
npm install --package-lock=false >/tmp/sybra-testbed-npm.log 2>&1
PORT=3010 node server.js >/tmp/sybra-testbed.log 2>&1 &
pid=\$!
trap 'kill \$pid 2>/dev/null || true' EXIT
sleep 1
test \"\$(curl -fsS http://127.0.0.1:3010/version)\" = '1.0.0'
test \"\$(curl -fsS http://127.0.0.1:3010/)\" = 'sybra-testbed ok'
test \"\$(curl -fsS http://127.0.0.1:3010/healthz)\" = 'ok'
"
```

The proven run created `job/sybra-agent-952d20d5`, used provider `opencode`
with model `openrouter/deepseek/deepseek-v4-flash`, committed only `server.js`,
and cost `0.000499518` USD according to Sybra's parsed OpenCode metadata.
