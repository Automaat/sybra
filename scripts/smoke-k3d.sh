#!/usr/bin/env bash
# Deterministic k3d smoke for the Kubernetes agent-Job runner.
#
# Automates deploy/k8s-poc/README.md's "Fake repo e2e mode": stands up a k3d
# cluster, deploys sybra-server, seeds a project backed by a local bare repo on
# the PVC, and runs one headless agent as a Kubernetes Job. The Job runs
# fake-claude, so the whole path is exercised with no model API keys and no
# provider spend.
#
# Five things the Job runner has to get right, each asserted below:
#   1. Job spawn      — a sybra-agent-<id> Job is created and completes
#   2. Log parsing    — Sybra parses the pod's NDJSON (cost/tokens/session)
#   3. Commit         — the wrapper commits the agent's file change
#   4. Push-back      — the branch is pushed to the PVC-backed origin
#   5. Fast-forward   — the server-side worktree picks the change up
#
# Never touches the caller's kubectl context: the cluster is created with
# --kubeconfig-update-default=false and every command runs against an isolated
# kubeconfig in a temp dir.
#
# SMOKE_PROVIDER=opencode swaps fake-claude for the real OpenCode CLI against
# OpenRouter. That path costs money and needs OPENROUTER_API_KEY, so it is
# opt-in and never runs on a PR — see .github/workflows/k3d-provider-smoke.yml.
# It exists because the fake path can never prove the real provider CLI launches
# inside the agent image: the wrapper swaps argv[0] for SYBRA_K8S_PROVIDER_BIN,
# so fake-claude replaces the very launch it would need to exercise.
#
# Usage:
#   scripts/smoke-k3d.sh              # full run, then tear down
#   SMOKE_KEEP=1 scripts/smoke-k3d.sh # leave the cluster up for debugging
#   SMOKE_SKIP_BUILD=1 ...            # reuse an existing sybra-server:poc
#   SMOKE_PROVIDER=opencode ...       # real OpenRouter run (costs money)
#   SMOKE_GITHUB=1 ...                # push to the real GitHub testbed + open a PR
set -euo pipefail

PROVIDER="${SMOKE_PROVIDER:-fake}"
# GitHub mode swaps the PVC-backed bare clone for the real testbed remote, which
# is the only way to prove the Job pushes to GitHub and opens a PR. Still runs
# fake-claude, so it costs nothing but GitHub API calls.
GITHUB_MODE="${SMOKE_GITHUB:-0}"
TESTBED="${SMOKE_TESTBED:-Automaat/sybra-testbed}"
CLUSTER="${SMOKE_CLUSTER:-sybra-smoke}"
NS=sybra-poc
IMAGE=sybra-server:poc
KEEP="${SMOKE_KEEP:-0}"
TIMEOUT="${SMOKE_TIMEOUT:-240}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

WORK="$(mktemp -d)"
export KUBECONFIG="$WORK/kubeconfig"

log() { printf '\n=== %s\n' "$*"; }
fail() { printf '::error::%s\n' "$*" >&2; exit 1; }

kc() { kubectl --context "k3d-$CLUSTER" -n "$NS" "$@"; }
in_pod() { kc exec deploy/sybra-server -- "$@"; }

dump_diagnostics() {
  printf '\n===== DIAGNOSTICS =====\n' >&2
  kc get pods,jobs -o wide 2>&1 | head -30 >&2 || true
  # A Pending pod says nothing about why; the events do (PVC binding, taints,
  # insufficient memory). Cheap, and the first thing wanted on a CI failure.
  printf '\n--- recent events ---\n' >&2
  kc get events --sort-by=.lastTimestamp 2>&1 | tail -15 >&2 || true
  # Sybra's slog handler writes to a rotating file, not stdout, so `kubectl logs`
  # on the server shows almost nothing — the real sink is on the PVC.
  in_pod tail -100 /home/sybra/.sybra/logs/sybra.log 2>&1 | tail -60 >&2 || true
  printf '\n--- agent job pods ---\n' >&2
  for p in $(kc get pods -l app.kubernetes.io/name=sybra-agent -o name 2>/dev/null); do
    printf '\n--- %s ---\n' "$p" >&2
    kc logs "$p" 2>&1 | tail -30 >&2 || true
  done
}

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then dump_diagnostics || true; fi
  if [ "$KEEP" = "1" ]; then
    printf '\nSMOKE_KEEP=1 — cluster %s left up. KUBECONFIG=%s\n' "$CLUSTER" "$KUBECONFIG" >&2
  else
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
  exit "$rc"
}
trap cleanup EXIT

for bin in docker k3d kubectl jq; do
  command -v "$bin" >/dev/null 2>&1 || fail "$bin is required but not on PATH"
done

if [ "$GITHUB_MODE" = "1" ]; then
  command -v gh >/dev/null 2>&1 || fail "SMOKE_GITHUB=1 needs gh on PATH to assert the branch and PR"
fi

if [ "${SMOKE_SKIP_BUILD:-0}" != "1" ]; then
  log "Building $IMAGE"
  docker build -t "$IMAGE" "$REPO_ROOT"
fi

# Single node on purpose: `k3d image import` loads the image into every node, and
# the agent image is multi-GB (node + three provider CLIs), so `--agents 1`
# imported it twice and OOM-killed the node on a CI runner (exit 137, then the
# API server fell over). This smoke exercises the Job runner, not multi-node
# scheduling, so the extra node bought nothing.
log "Creating cluster $CLUSTER"
k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
k3d cluster create "$CLUSTER" --agents 0 --wait \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG"

log "Importing $IMAGE"
k3d image import "$IMAGE" -c "$CLUSTER"

# Resolve the config BEFORE anything is applied. The old order applied the
# kustomization (starting a pod on the default config), swapped the ConfigMap,
# then rollout-restarted — because the config is a subPath mount, which never
# updates in place. That restart raced itself: strategy Recreate plus a
# ReadWriteOnce PVC means the new pod cannot schedule until the old one releases
# the volume, and the old one was still coming up. It hung Pending until the
# rollout timed out. Giving the pod the right config at birth removes the
# restart, and with it the race.
case "$PROVIDER" in
  fake) CONFIG=config-fake-repo.yaml ;;
  opencode)
    CONFIG=config-provider-example.yaml
    [ -n "${OPENROUTER_API_KEY:-}" ] || fail "SMOKE_PROVIDER=opencode needs OPENROUTER_API_KEY"
    ;;
  *) fail "unknown SMOKE_PROVIDER '$PROVIDER' (want: fake, opencode)" ;;
esac
if [ "$GITHUB_MODE" = "1" ]; then
  CONFIG=config-github-testbed.yaml
  [ -n "${GITHUB_TOKEN:-}" ] || fail "SMOKE_GITHUB=1 needs GITHUB_TOKEN (contents+pull-requests write on $TESTBED)"
fi

log "Applying manifests ($PROVIDER, config=$CONFIG)"
kubectl --context "k3d-$CLUSTER" apply -f "$REPO_ROOT/deploy/k8s-poc/namespace.yaml"

if [ "$GITHUB_MODE" = "1" ] || [ "$PROVIDER" = "opencode" ]; then
  kc create secret generic sybra-provider-api-keys \
    --from-literal=openrouter_api_key="${OPENROUTER_API_KEY:-}" \
    --from-literal=github_token="${GITHUB_TOKEN:-}" \
    --dry-run=client -o yaml | kubectl --context "k3d-$CLUSTER" apply -f -
fi

# The chosen ConfigMap goes in first, then everything else except the default
# one it would otherwise be clobbered by (all three declare the same name).
kubectl --context "k3d-$CLUSTER" apply -f "$REPO_ROOT/deploy/k8s-poc/$CONFIG"
kubectl kustomize "$REPO_ROOT/deploy/k8s-poc" \
  | python3 -c '
import sys
docs = sys.stdin.read().split("\n---\n")
kept = [d for d in docs if not ("kind: ConfigMap" in d and "name: sybra-config" in d)]
sys.stdout.write("\n---\n".join(kept))
' | kubectl --context "k3d-$CLUSTER" apply -f -

kc rollout status deployment/sybra-server --timeout=180s

if [ "$GITHUB_MODE" = "1" ]; then
  log "Registering the real testbed project ($TESTBED)"
  PROJECT="$TESTBED"
  in_pod sybra-cli project create --url "https://github.com/$TESTBED.git" >/dev/null
else
  PROJECT=FakeOrg/k8s-testbed
  log "Seeding fake repo project"
  in_pod sh -ceu '
HOME_DIR=/home/sybra/.sybra
OWNER=FakeOrg
REPO=k8s-testbed
SRC=/tmp/k8s-testbed-src
BARE="$HOME_DIR/clones/$OWNER/$REPO.git"

rm -rf "$SRC" "$BARE"
mkdir -p "$HOME_DIR/clones/$OWNER" "$HOME_DIR/projects"
git init -b main "$SRC"
git -C "$SRC" config user.email fake-repo@example.invalid
git -C "$SRC" config user.name "Fake Repo"
printf "hello from fake repo\n" > "$SRC/README.md"
git -C "$SRC" add README.md
git -C "$SRC" commit -q -m "chore: seed fake repo"
git clone -q --bare "$SRC" "$BARE"
git --git-dir="$BARE" config remote.origin.url "$BARE"
git --git-dir="$BARE" config remote.origin.fetch "+refs/heads/*:refs/remotes/origin/*"
git --git-dir="$BARE" config receive.denyCurrentBranch updateInstead
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
fi

# A real model needs the literal spelled out; fake-claude ignores the prompt and
# writes the same marker either way, so both providers land on one assertion set.
PROMPT="Create a file named k8s-agent-output.txt in the repository root, containing exactly this line and nothing else: changed by k8s fake agent"

log "Creating task"
TASK_ID=$(in_pod sybra-cli --json create \
  --title "k3d smoke: fake repo agent job" \
  --body "$PROMPT" \
  --mode headless --project "$PROJECT" \
  --tags handoff-manual,k8s-smoke --allow-dup | jq -r .id)
[ -n "$TASK_ID" ] && [ "$TASK_ID" != "null" ] || fail "could not create task"
echo "task: $TASK_ID"

# Deliberately App.StartAgent, not the task-created workflow: this smoke proves
# the Job backend, so it must not depend on triage/planning behaviour. The
# shipped config is orchestrator.role=agent-only, which never auto-dispatches.
log "Starting agent"
in_pod curl -sS -X POST 'http://127.0.0.1:8080/api/App/StartAgent' \
  -H 'Authorization: Bearer poc-token' -H 'Content-Type: application/json' \
  --data "$(jq -nc --arg t "$TASK_ID" --arg p "$PROMPT" '[$t,"headless",$p,true]')" >/dev/null

# `kubectl wait -l <selector>` does NOT wait for a resource to appear: with no
# match it exits 1 immediately and ignores --timeout. StartAgent returns as soon
# as the agent is registered and the Job is POSTed later from a goroutine (after
# a git detect + push), so the Job reliably does not exist yet here. Poll for it.
log "Waiting for the agent Job"
JOB=""
for _ in $(seq 1 60); do
  JOB=$(kc get job -l app.kubernetes.io/name=sybra-agent -o name 2>/dev/null | head -1)
  [ -n "$JOB" ] && break
  sleep 1
done
[ -n "$JOB" ] || fail "no agent Job appeared within 60s"

# Polled rather than `kubectl wait --for=condition=complete`: with backoffLimit 0
# a failed Job never satisfies `complete`, so a real failure would burn the whole
# timeout before reporting. This reports it immediately.
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  succeeded=$(kc get "$JOB" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
  failed=$(kc get "$JOB" -o jsonpath='{.status.failed}' 2>/dev/null || true)
  [ "${succeeded:-0}" != "0" ] && [ -n "${succeeded:-}" ] && break
  if [ "${failed:-0}" != "0" ] && [ -n "${failed:-}" ]; then
    fail "agent Job $JOB failed"
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "agent Job $JOB did not finish within ${TIMEOUT}s"
  fi
  sleep 2
done

# A completed Job is not a finalized agent: the runner polls on a 750ms ticker
# and still has to drain the pod's final logs, fast-forward the worktree, and
# record the result. Asserting straight off `kubectl wait` races that and reads
# state=running.
log "Waiting for the agent to finalize"
AGENT=""
finalize_deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  AGENT=$(in_pod curl -sS -X POST 'http://127.0.0.1:8080/api/AgentService/ListAgents' \
    -H 'Authorization: Bearer poc-token' -H 'Content-Type: application/json' --data '[]' \
    | jq -c --arg t "$TASK_ID" '[.[] | select(.taskId == $t)] | last')
  [ "$(echo "$AGENT" | jq -r '.state // empty')" = "stopped" ] && break
  [ "$(date +%s)" -ge "$finalize_deadline" ] && break
  sleep 2
done
[ "$AGENT" != "null" ] && [ -n "$AGENT" ] || fail "no agent recorded for task $TASK_ID"

log "Asserting"
echo "$AGENT" | jq .

check() {
  local label="$1" expr="$2" want="$3"
  local got
  got=$(echo "$AGENT" | jq -r "$expr")
  [ "$got" = "$want" ] || fail "$label = '$got', want '$want'"
  printf '  ok  %s = %s\n' "$label" "$got"
}

# 1. Job spawn — the agent ran as a Kubernetes Job, not a local subprocess.
check "command prefix" '.command | startswith("kubernetes job/")' "true"
check "state" '.state' "stopped"

# 2. Log parsing — these values exist only if Sybra parsed the pod's NDJSON.
if [ "$PROVIDER" = "fake" ]; then
  # fake-claude's result event reports exactly this triple, so a wrong number
  # means the stream parser broke, and 0 would mean the runner silently fell
  # back to fake mode's inline script instead of running the provider.
  check "provider" '.provider' "claude"
  check "costUsd" '.costUsd' "0.01"
  check "inputTokens" '.inputTokens' "100"
  check "outputTokens" '.outputTokens' "50"
else
  # A real model's numbers are not fixed, so assert only that the parser
  # extracted them at all: 0/absent means the result event was never read.
  check "provider" '.provider' "opencode"
  check "cost parsed" '(.costUsd // 0) > 0' "true"
  check "tokens parsed" '((.inputTokens // 0) > 0) and ((.outputTokens // 0) > 0)' "true"
fi

# Task.WorktreeDir is only populated for an adopted worktree (the handoff
# path); a normal agent run derives worktrees/<slug>-<id> and never writes the
# field back, so glob for it rather than reading it off the task.
WT=$(in_pod sh -ceu "ls -d /home/sybra/.sybra/worktrees/*$TASK_ID 2>/dev/null | head -1" | tr -d '\r')
[ -n "$WT" ] || fail "no worktree found for task $TASK_ID under ~/.sybra/worktrees"

if [ "$GITHUB_MODE" = "0" ]; then
  # 3/4/5. On the PVC path the server owns the worktree, so the marker file
  # reaching it proves the wrapper committed, pushed to the bare origin, and the
  # server fast-forwarded. GitHub mode asserts against GitHub instead: there the
  # Job owns the repo work and the server-side fast-forward is the very coupling
  # being removed, so the branch on the remote is the honest evidence.
  in_pod sh -ceu "test -f '$WT/k8s-agent-output.txt'" \
    || fail "k8s-agent-output.txt missing from the server worktree — push-back or fast-forward failed"
  in_pod grep -q 'changed by k8s fake agent' "$WT/k8s-agent-output.txt" \
    || fail "k8s-agent-output.txt has unexpected content"
  printf '  ok  marker file fast-forwarded into %s\n' "$WT"

  in_pod git -C "$WT" log --oneline -3 | grep -q 'persist k8s agent changes' \
    || fail "no 'chore: persist k8s agent changes' commit in the worktree — the wrapper did not commit"
  printf '  ok  agent commit present\n'
fi

if [ "$GITHUB_MODE" = "1" ]; then
  BRANCH=$(in_pod git -C "$WT" rev-parse --abbrev-ref HEAD | tr -d '\r')
  [ -n "$BRANCH" ] || fail "could not read the task branch"

  # 6. The branch reached GitHub itself, not just the PVC. This is the whole
  #    point of the mode: the PoC only ever proved push-back to a bare clone.
  gh api "repos/$TESTBED/branches/$BRANCH" >/dev/null 2>&1 \
    || fail "branch $BRANCH is not on $TESTBED — the Job did not push to GitHub"
  printf '  ok  branch %s pushed to %s\n' "$BRANCH" "$TESTBED"

  # The agent's actual work is on that branch, not just an empty ref.
  gh api "repos/$TESTBED/contents/k8s-agent-output.txt?ref=$BRANCH" --jq '.content' 2>/dev/null \
    | base64 -d 2>/dev/null | grep -q 'changed by k8s fake agent' \
    || fail "k8s-agent-output.txt is not on $BRANCH at $TESTBED — the Job pushed an empty branch"
  printf '  ok  agent work is on the pushed branch\n' 

  # 7. The Job opened the PR itself and recorded it, so the server never needed
  #    a GitHub credential.
  PR=$(in_pod sybra-cli --json get "$TASK_ID" | jq -r '.prNumber // 0')
  [ "${PR:-0}" -gt 0 ] 2>/dev/null \
    || fail "task has no pr_number — the Job did not open a PR (or could not report it back)"
  gh api "repos/$TESTBED/pulls/$PR" --jq '.state' 2>/dev/null | grep -q open \
    || fail "PR #$PR is not open on $TESTBED"
  printf '  ok  Job opened PR #%s and recorded it on the task\n' "$PR"

  log "Cleaning up the testbed"
  gh pr close "$PR" --repo "$TESTBED" --delete-branch --comment "k3d smoke run; closing automatically." >/dev/null 2>&1 \
    || printf '::warning::could not close PR #%s on %s — close it by hand\n' "$PR" "$TESTBED"
fi

log "PASS — k3d agent-Job smoke green"
