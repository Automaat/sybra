# Sybra agent daemon

`sybra-agentd` is an outbound-only execution worker. It does not initialize a
Sybra board, task or project stores, workflows, GitHub automation, loop agents,
monitoring/evaluation services, or the UI/API server.

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
for committed descendants, a binary patch for staged/unstaged tracked changes,
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
Git mutation. Stale or corrupt packages never update the canonical worktree.

`sandbox_mode: enforce` fails each run closed if the host containment mechanism
or workspace profile cannot be established. `report` is accepted for rollout
diagnostics but does not claim containment. Provider health, OS/architecture,
capacity, sandbox posture, labels, models, build/protocol versions, and trusted
work eligibility are advertised during registration.
