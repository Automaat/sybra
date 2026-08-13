# Sybra agent daemon

`sybra-agentd` is an outbound-only execution worker. It does not initialize a
Sybra board, task or project stores, workflows, GitHub automation, loop agents,
monitoring/evaluation services, or the UI/API server.

## Leader authority and legacy migration

On a database-backed `cluster.role: leader` board, every workflow `run_agent`
effect is now placed independently through the worker scheduler. The leader
keeps the only canonical task, Agent record, workflow effect claim, budget,
sidecars, history, and completion callback. A daemon receives an immutable run
spec and returns ordered events plus a handback package; it never receives or
persists a task document. If no eligible daemon exists, the scheduler records
an explicit local decision and the same leader-owned completion path runs the
provider locally.

The older `cluster.followers` task-assignment and snapshot-mirroring mode is
deprecated. Existing fields remain loadable for rollback, and existing task
`assigned_node` metadata acts as placement affinity when the matching daemon
uses that worker ID, but a database-backed leader no longer pushes canonical
task snapshots to those nodes. Install `sybra-agentd` on each
execution node, point `leader_url` at the leader's `/worker/v1` service, copy
repository mirrors into the daemon's `repositories` map, then remove the old
follower service after its in-flight task runs have drained.

Start it with a dedicated YAML file and a leader token supplied only through an
environment variable:

```yaml
leader_url: https://leader.example.test
token_env: SYBRA_AGENTD_TOKEN
capacity: 2
providers: [claude, codex]
models: [sonnet, gpt-5.4]
labels: { zone: home }
trusted_work: false
encrypted_work: false
warm_caches: [go-build]
sandbox_mode: enforce
workspace_root: /var/lib/sybra-agentd/workspaces
state_root: /var/lib/sybra-agentd/state
spool_max_bytes: 67108864
workspace_retention_hours: 168
repositories:
  example-repo: /var/lib/sybra-agentd/clones/example.git
secret_env:
  run/example/provider-key: ANTHROPIC_API_KEY
```

```bash
SYBRA_AGENTD_TOKEN=... sybra-agentd -config /etc/sybra-agentd.yaml
```

The leader URL must use HTTPS except on loopback. `token_env` and `secret_env`
name environment variables; secret values are never written to the config.
The leader token is stripped from every provider subprocess. Run-scoped secret
references are resolved locally and only the requested provider environment
binding is injected.

Repository locations are daemon-local. The wire contract carries only the
opaque `repositories` key and an immutable full base SHA. For every accepted
run the daemon clones that mapping into an isolated logical `worktree` root,
verifies the SHA is reachable from the declared base ref, and checks out the
SHA detached; a ref that moves after dispatch cannot change the run's input.
Agent-facing paths are supplied through `SYBRA_WORKTREE_ROOT`,
`SYBRA_SIDECAR_ROOT`, `SYBRA_ARTIFACT_ROOT`, and
`SYBRA_WORKING_MEMORY_ROOT`, never leader host paths.

The state root contains a stable generated node identity, the restart-survival
process registry, the local approval endpoint identity, and an atomically
rewritten bounded spool. Event and artifact delivery is at least once. If the
spool limit is reached, the daemon reports `agentd: durable spool exhausted`
and stops the affected run rather than dropping output. Capacity overflow and
draining reject new starts with an explicit terminal event. Existing runs may
finish while draining.

Completion produces one deterministic, content-addressed package: a Git bundle
for committed descendants, separate binary patches preserving staged and
unstaged tracked changes,
sorted untracked blobs with portable modes, and only outputs declared by the
run. Each member has a size and SHA-256 digest; packages are bounded to 512
members, 32 MiB each, and 128 MiB total. `NOTES.md` and evidence scratch are
Git-excluded before execution. Working memory is returned only through an
explicit private output on author roles and is never part of Git handback.

The daemon retains a workspace and its durable upload until the leader
acknowledges the complete package, then deletes both. Unacknowledged or stale
diagnostic handbacks are retained for `workspace_retention_hours` (seven days
by default) and then reaped. The leader stores uploads as `staged`, not ready:
workflow advancement waits for a generation-fenced importer to verify package
membership/hashes, exact base ancestry, and a clean canonical base before any
Git mutation. The importer holds the worktree manager's canonical mutation
lock across both generation checks, quarantine validation, Git publication,
and declared-output import. Stale/corrupt handbacks are marked rejected and
retained privately; accepted Git state, sidecars, evidence/artifacts, and
private working memory flow into their leader-owned stores. Neither outcome is
workflow completion by itself.

`sandbox_mode: enforce` fails each run closed if the host containment mechanism
or workspace profile cannot be established. `report` is accepted for rollout
diagnostics but does not claim containment. Provider health, OS/architecture,
capacity, sandbox posture, labels, models, mapped repositories, warm-cache
hints, build/protocol versions, and trusted/encrypted work eligibility are
advertised during registration.

The leader schedules each run independently. `node_override` is a hard pin and
fails clearly when that node is unavailable; the legacy `assigned_node` value
is treated as an affinity and may fall back when policy allows. Active session
rows are locked while the fenced run and start command are persisted, so the
same transaction reserves advertised capacity. Terminal runs release capacity
idempotently. Draining or disabled nodes finish accepted work but receive no
new placements; diagnostics show their state, active and available capacity,
and placement results carry per-candidate rejection reasons. A caller must opt
in explicitly when no eligible daemon may fall back to local execution.
