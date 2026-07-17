# Sybra Kubernetes agent runner PoC

Runs `sybra-server` as a Kubernetes Deployment and routes headless agents to
short-lived Kubernetes Jobs. This is a smoke-test harness, not a production
deployment.

## Run locally with k3d

```bash
docker build -t sybra-server:poc .
k3d cluster create sybra-poc --agents 1 --wait
k3d image import sybra-server:poc -c sybra-poc
kubectl apply -f deploy/k8s-poc/namespace.yaml
kubectl -n sybra-poc create secret generic sybra-server-auth \
  --from-literal=token=poc-token \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/k8s-poc
kubectl -n sybra-poc rollout status deployment/sybra-server --timeout=120s
```

`sybra-server-auth` holds the bearer token the Deployment reads via
`SYBRA_AUTH_TOKEN` (`deploy/k8s-poc/deployment.yaml`) — it is never a plain
manifest value, so `kubectl apply -k` alone will not start the pod until this
Secret exists. See [Production secret management](#production-secret-management)
for how to handle it (and `sybra-provider-api-keys` below) outside a
throwaway k3d cluster.

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

## Automated smoke

`scripts/smoke-k3d.sh` runs the "Fake repo e2e mode" flow below end to end and
asserts it, so the Job runner has regression cover without a model API key or
any provider spend. CI runs it on every PR as the `k3d Job Runner Smoke` job.

```bash
mise run smoke:k3d              # build, run, tear down
SMOKE_KEEP=1 mise run smoke:k3d # leave the cluster up to poke at it
```

It needs `docker`, and `k3d`/`kubectl` come from `mise install`. It never
touches your kubectl context: the cluster is created with
`--kubeconfig-update-default=false` and every command runs against an isolated
kubeconfig, so a `sybra-poc` cluster or a work context nearby is safe.

Deliberately excluded from `mise run verify`, which stays a fast deterministic
pre-commit loop — this needs Docker and a real cluster.

For a real-model run against OpenRouter, see the manual
`.github/workflows/k3d-provider-smoke.yml` (needs an `OPENROUTER_API_KEY`
secret) or run `SMOKE_PROVIDER=opencode OPENROUTER_API_KEY=... mise run
smoke:k3d` locally. That path costs money, so it never runs on a PR.

## Fake repo e2e mode

The manual version of what the smoke automates. Use this path when you want a
fully local k3d test that exercises the real
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

WT=$(
  kubectl -n sybra-poc exec deploy/sybra-server -- \
    sh -ceu "ls -d /home/sybra/.sybra/worktrees/*$TASK_ID | head -1"
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

## Production secret management

The PoC keeps two Secrets out of every manifest and out of git:

- `sybra-server-auth` (`deploy/k8s-poc/server-auth-secret.example.yaml`) — the
  Deployment's `SYBRA_AUTH_TOKEN`.
- `sybra-provider-api-keys` (`deploy/k8s-poc/api-key-secret.example.yaml`) —
  provider/GitHub credentials referenced by `agent.k8s_jobs.secret_env` and
  injected straight into the agent Job's container as `valueFrom.secretKeyRef`
  (`internal/agent/k8s_job_runner.go`). Kubernetes resolves the reference at
  pod start; `sybra-server` never reads the secret value itself, so a
  compromised server process cannot exfiltrate provider or GitHub credentials
  it was never handed. `sybra-cli config doctor` catches a `secret_env` entry
  with a missing `name`/`secret_name`/`secret_key` before it fails silently at
  Job-creation time.

The `.example.yaml` files are templates only (`replace-me` placeholders) and
are not part of `kustomization.yaml` — nothing applies them automatically, and
`kubectl create secret generic ... --from-literal=...` never writes a
manifest to disk. That is enough for a throwaway k3d cluster. A real
deployment needs the Secret's desired state to be reviewable and
reproducible without ever putting plaintext credentials in git history. Pick
one:

- **SOPS** — encrypt a Secret manifest (or just the values) with `sops`,
  commit the ciphertext, decrypt at deploy time and `kubectl apply` the
  result (or `kubectl create secret --from-literal` from the decrypted
  values, as `server-auth-secret.example.yaml` and
  `api-key-secret.example.yaml` are shaped for). This is the option to
  reach for if your deploy pipeline already runs `sops` for other
  environments — the encrypted file lives next to the rest of the manifests
  and needs no extra cluster component.
- **Sealed Secrets** (bitnami-labs/sealed-secrets) — a cluster-side
  controller decrypts a `SealedSecret` CRD that is safe to commit (encrypted
  with the controller's public key) into a real `Secret`. Best fit when the
  encryption key must stay cluster-local rather than shared with a CI
  pipeline.
- **External Secrets Operator** — a cluster-side controller syncs Secrets
  from an external store (Vault, AWS/GCP/Azure secret managers, etc.) via an
  `ExternalSecret` CRD; nothing encrypted ever touches git. Best fit when
  provider/GitHub credentials are already centrally managed in one of those
  stores.

Whichever mechanism renders the final `Secret`, the object names/keys must
match what `deployment.yaml` and `agent.k8s_jobs.secret_env` expect:
`sybra-server-auth`/`token`, and `sybra-provider-api-keys`/
`openrouter_api_key`+`github_token` (or whatever `secret_name`/`secret_key`
pairs a given ConfigMap declares). None of the three tools are wired into
this PoC's kustomization — pick one and add its manifests/controller to your
own deployment overlay.

## Versioned production images

`docker build -t sybra-server:poc .` + `k3d image import` above is a local
dev/smoke loop only — the resulting image never leaves the machine that built
it, which is fine for a throwaway k3d cluster but not reproducible or
immutable enough to run in production.

`.github/workflows/docker-publish.yml` builds the same root `Dockerfile` (its
`ENTRYPOINT` is `sybra-server`) and pushes it to GHCR under two repository
names sharing one digest per build: `ghcr.io/automaat/sybra` and
`ghcr.io/automaat/sybra-server`. Each push is tagged three ways:

- `vX.Y.Z` — the exact released version
- `X.Y` — floating latest-patch alias for that minor
- `sha-<short-sha>` — the commit it was built from, for bisecting a bad
  deploy back to source independent of the version bump

It runs on `workflow_dispatch` (`gh workflow run docker-publish.yml -f
version=vX.Y.Z`, or leave `version` empty to auto-bump the minor via
`scripts/resolve-release-version.sh`) — publishing a new production image is
a deliberate, CI-run action, not something that fires on every merge to
`main`.

The first push under a new GHCR repository name defaults to **private** —
`ghcr.io/automaat/sybra-server` will not exist as a package until
`docker-publish.yml` runs once, and after that first run it is only pullable
by an authenticated, authorized principal until someone flips its visibility
in the package's GHCR settings (Package → Package settings → Danger Zone →
Change visibility). Decide deliberately rather than by default:

- **Public** — simplest; an unauthenticated `docker pull` (as shown below)
  and a plain Kubernetes image reference both just work, no cluster-side
  credential needed.
- **Private** — safer default for a production image, but then the cluster
  needs an `imagePullSecrets` entry: `kubectl create secret docker-registry
  ghcr-pull --docker-server=ghcr.io --docker-username=<user>
  --docker-password=<a token with read:packages>`, referenced from the
  Deployment's `spec.template.spec.imagePullSecrets` (or the ServiceAccount,
  so every pod using it inherits the credential without repeating the field).

For a production deploy, pin by digest rather than a mutable tag — a tag can
be re-pointed (accidentally or via `docker-publish.yml` re-running the same
version), a digest cannot. If the package is still private, log in first
(a token with `read:packages` is enough — no need for `write:packages` just
to pull):

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u <user> --password-stdin
docker pull ghcr.io/automaat/sybra-server:vX.Y.Z
docker inspect --format='{{index .RepoDigests 0}}' ghcr.io/automaat/sybra-server:vX.Y.Z
# ghcr.io/automaat/sybra-server@sha256:<digest>
```

Point a deployment at it without ever building locally — either patch the
image directly:

```bash
kubectl -n sybra-poc set image deployment/sybra-server \
  sybra-server=ghcr.io/automaat/sybra-server@sha256:<digest>
```

or, in a kustomize overlay (see issue #2110 for splitting the PoC base from a
real production overlay), add an `images:` transformer instead of hand-editing
`deployment.yaml`:

```bash
cd your-production-overlay
kustomize edit set image sybra-server:poc=ghcr.io/automaat/sybra-server@sha256:<digest>
```

## Job cleanup and log retention

Every agent Job runs with `backoffLimit: 0` and `restartPolicy: Never`
(`deploy/k8s-poc/deployment.yaml` config, applied in
`internal/agent/k8s_job_runner.go`), so it reaches a terminal Kubernetes Job
condition — Complete or Failed — on its first pod exit, success or failure
alike. Kubernetes' [TTL-after-finished
controller](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/)
then deletes the Job (and, via its pod's ownerReference, the Pod) once
`spec.ttlSecondsAfterFinished` elapses from that terminal condition — deleting
the Pod is also what makes `kubectl logs` on it stop working, so this number
is really "how long do I have to pull a Job's logs before they're gone."

**Two separate defaults, not one**, because a five-minute window is fine for
a Job that worked but far too short to debug one that didn't:

- `agent.k8s_jobs.ttl_seconds_after_finished` — applied at Job creation,
  covers the **successful** case. Default **300s** if unset.
- `agent.k8s_jobs.failed_ttl_seconds_after_finished` — Kubernetes has no
  native way to set a different TTL for Failed vs Complete at creation time
  (`ttlSecondsAfterFinished` is one scalar), so the runner instead PATCHes it
  onto the Job the moment it observes `status.failed > 0`, before finalizing
  the agent run. Default **86400s (24h)** if unset — long enough that an
  operator investigating a page the next morning still has the evidence.
  Requires the `patch` verb on `batch/jobs` in the Role bound to
  `sybra-server`'s ServiceAccount (already granted in
  `deploy/k8s-poc/rbac.yaml`); without it the patch attempt is logged as a
  warning (`agent.k8s.failed_ttl_patch`) and the Job silently falls back to
  the success TTL.

Both are configurable per-deployment via the `agent.k8s_jobs` ConfigMap key —
see `deploy/k8s-poc/config-provider-example.yaml` for the surrounding block.

**Inspecting a failed run before it's cleaned up:**

```bash
# Sybra's own parsed view — cost, tokens, session id, exit state
kubectl -n sybra-poc exec deploy/sybra-server -- \
  curl -sS -X POST 'http://127.0.0.1:8080/api/AgentService/ListAgents' \
    -H 'Authorization: Bearer poc-token' -H 'Content-Type: application/json' --data '[]' \
  | jq '.[] | select(.taskId == "<task-id>")'

# Raw provider stdout/stderr for the pod (works until failedTTL elapses)
kubectl -n sybra-poc logs job/sybra-agent-<agent-id>

# Why the pod itself failed (OOMKilled, image pull error, etc. — not just
# a nonzero exit from the provider CLI)
kubectl -n sybra-poc describe pod -l sybra.agent/id=<agent-id>
```

**Stale Jobs** — a Job that exists in the cluster but no longer maps to an
agent Sybra is actively tracking, typically after a server crash/restart
during a run.

For the common case — the orphaned Job still finishes on its own — TTL
already covers it: `ttlSecondsAfterFinished` deletes a Job once it reaches
Complete/Failed regardless of whether anything in Sybra still tracks its
`sybra.agent/id`, since Kubernetes' TTL controller only looks at the Job's
own status, never at Sybra's registry. A restart doesn't stop the pod that's
already running; it just orphans Sybra's reference to it, and the two TTLs
above still reap it on the same schedule as a normally-tracked run.

What TTL *cannot* reach is a Job that never finishes at all — stuck
`Pending`/`Running` forever with no terminal condition to start either TTL's
countdown. That's a timeout/liveness problem, not a retention-window problem,
and it's tracked separately: #2106 (resource/timeout/concurrency controls)
and #2109 (observability and stuck-Job alerts) own detecting and acting on a
Job that's actually hung, rather than one that finished and is just waiting
out its TTL.

What's left for a human to do manually today is inspection, not cleanup: every
agent Job carries `app.kubernetes.io/name=sybra-agent`, `sybra.agent/id=<id>`,
and `sybra.task/id=<id>` labels (`internal/agent/k8s_job_runner.go`), so an
operator can list Jobs with that selector and cross-reference the ids against
`ListAgents`/`sybra-cli list` to see what's currently orphaned before its TTL
elapses:

```bash
kubectl -n sybra-poc get jobs -l app.kubernetes.io/name=sybra-agent \
  -o jsonpath='{range .items[*]}{.metadata.labels.sybra\.agent/id}{"\n"}{end}'
```

Automating that lookup into a scheduled sweep (rather than an operator running
it by hand), and extending the same garbage collection to branches and
worktrees left behind by an orphaned run, is #2111. Reattaching Sybra's own
tracking to a still-running Job after a restart — so it stops looking
"orphaned" from Sybra's side in the first place — is #2112.

Automatically *detecting and pruning* stale Jobs — not just finding them by
hand — is out of scope here; it's tracked in #2111. Automatically
*reattaching* to a Job that outlived a server restart, so it stops looking
stale in the first place, is #2112. This section defines the retention
policy and the manual inspection path; those two issues own the automation.

## Persistent storage, backup, and restore

`SYBRA_HOME` holds several kinds of state with very different durability
needs, so it's split across three PVCs rather than the one this PoC started
with:

| PVC | Mount | Holds | Reproducible? |
|---|---|---|---|
| `sybra-home` | `/home/sybra/.sybra` | `tasks/`, `projects/`, `config.yaml`, `agents/` (live-agent registry), `learning/`, `experience/`, `agentqueue/`, `logs/`, `artifacts/` | **No** — this is Sybra's actual ledger: task/project history, the thing the issue's acceptance criterion means by "state" |
| `sybra-clones` | `/home/sybra/.sybra/clones` | bare git clones, one per registered project | Yes — re-clonable from each project's remote |
| `sybra-worktrees` | `/home/sybra/.sybra/worktrees` | per-task working-tree checkouts | Yes — re-derivable from a clone + task branch, modulo any uncommitted/unpushed changes in a worktree that dies mid-run (a risk independent of backup strategy) |

Only `sybra-home` needs backup coverage. `sybra-clones`/`sybra-worktrees` are
capacity-planning concerns (they grow with registered-project count and
concurrent-task count), not data-loss concerns — losing them costs re-clone
time, not history. `sybra-home` carries the label `sybra.io/backup: "true"`
so a backup job can select it (`kubectl get pvc -n sybra-poc -l
sybra.io/backup=true`) without hardcoding its name.

**Storage class.** None of the three PVCs sets `storageClassName` — they use
whatever the cluster's default StorageClass is, which for this PoC's
documented k3d workflow is k3d's bundled `local-path-provisioner`. That
provisioner backs a PV with a plain `hostPath` directory on whichever node
the pod lands on: no CSI `VolumeSnapshot` support (there's no driver for a
`VolumeSnapshotClass` to target), and the PV is only accessible from the node
that created it — consistent with `accessModes: [ReadWriteOnce]` and this
PoC's single-replica Deployment. A production cluster should instead point
these PVCs at a real CSI-backed block-storage class (`gp3`/EBS on EKS, PD-SSD
on GKE, Longhorn, Ceph-CSI, ...) via a production overlay's `storageClassName`
patch (see #2110) — that unlocks `VolumeSnapshot`/`VolumeSnapshotClass` for
fast, storage-layer, largely-automatic backups instead of the manual
tar-based procedure below, which is the *only* option `local-path` leaves you.

**Consistency.** Every task/project file is written via a temp-file-then-rename
(`internal/fsutil.AtomicWrite`), so a live filesystem-level backup never
captures a torn write — but it's a crash-consistent snapshot, not a
transactionally-consistent one: two files written a moment apart could still
land on either side of the backup instant. Good enough to restore and keep
going (Sybra always re-reads from disk, nothing depends on an in-memory
invariant spanning files); scale the Deployment to 0 first if you want a
stronger guarantee for a particular backup run.

**Backup** (works against `local-path`'s hostPath PVs; a real CSI driver's
`VolumeSnapshot` replaces this entirely):

```bash
kubectl -n sybra-poc exec deploy/sybra-server -- \
  tar czf - -C /home/sybra/.sybra tasks projects config.yaml agents learning experience agentqueue \
  > sybra-home-backup-$(date +%Y%m%d-%H%M%S).tar.gz
```

**Restore** into a fresh (or wiped) `sybra-home` PVC — scale down first so
nothing is writing to the volume while it's being repopulated:

```bash
kubectl -n sybra-poc scale deployment/sybra-server --replicas=0
kubectl -n sybra-poc run sybra-restore --rm -i --restart=Never \
  --image=busybox:1.36 --overrides='
{
  "spec": {
    "containers": [{
      "name": "sybra-restore",
      "image": "busybox:1.36",
      "command": ["sh", "-c", "tar xzf /backup/backup.tar.gz -C /home/sybra/.sybra && echo restored"],
      "volumeMounts": [
        {"name": "sybra-home", "mountPath": "/home/sybra/.sybra"},
        {"name": "backup", "mountPath": "/backup"}
      ]
    }],
    "volumes": [
      {"name": "sybra-home", "persistentVolumeClaim": {"claimName": "sybra-home"}},
      {"name": "backup", "hostPath": {"path": "/tmp/sybra-restore"}}
    ]
  }
}' -- true
kubectl -n sybra-poc scale deployment/sybra-server --replicas=1
```

(The `backup` `hostPath` above assumes a k3d/local cluster where the node can
see your local filesystem — copy the tarball to `/tmp/sybra-restore/backup.tar.gz`
on the node first, e.g. via `docker cp` into the k3d server container. A
production cluster restoring from off-cluster backup storage would instead
mount that storage, e.g. an S3-backed init container, in place of `hostPath`.)

`tasks-snapshots.git` — a background git-versioned history of the tasks
directory Sybra maintains on its own (`internal/tasksnapshot`, enabled by
default, commits on a 30s interval) — lives inside `sybra-home` and comes
back automatically with the restore above. It's a second line of defense
against accidental task deletion independent of any external backup, and
restoring from an external backup also restores it.

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
  sybra-cli project create --url https://github.com/Automaat/sybra-testbed.git
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
    sh -ceu "ls -d /home/sybra/.sybra/worktrees/*$TASK_ID | head -1"
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
