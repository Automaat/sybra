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

The state root contains a stable generated node identity, the restart-survival
process registry, the local approval endpoint identity, and an atomically
rewritten bounded spool. Event and artifact delivery is at least once. If the
spool limit is reached, the daemon reports `agentd: durable spool exhausted`
and stops the affected run rather than dropping output. Capacity overflow and
draining reject new starts with an explicit terminal event. Existing runs may
finish while draining.

`sandbox_mode: enforce` fails each run closed if the host containment mechanism
or workspace profile cannot be established. `report` is accepted for rollout
diagnostics but does not claim containment. Provider health, OS/architecture,
capacity, sandbox posture, labels, models, build/protocol versions, and trusted
work eligibility are advertised during registration.
