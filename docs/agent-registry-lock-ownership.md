# Agent Registry Lock Ownership

This note documents the lock boundary for restart-survival registry code before
any further agent lifecycle split. The registry persists live-agent snapshots in
`~/.sybra/agents/*.yaml` so a later app instance can reattach to detached
processes.

## Locks

| Lock | Owner | Guards | Does not guard |
| --- | --- | --- | --- |
| `Manager.mu` | `internal/agent/manager.go` | `Manager.agents`, `liveCount`, dispatch claims, runtime config, and survival config | Registry file reads/writes, agent-internal fields |
| `registryStore.mu` | `internal/agent/registry.go` | On-disk registry operations: `Save`, `List`, `Delete`, including FIFO cleanup in `Delete` | In-memory agent lifecycle state |
| `Agent.mu` | `internal/agent/model.go` | Per-agent mutable state copied into `Record` by `Agent.toRecord()` | Manager maps, registry directory contents |

## Registry Mutation Map

| Path | Registry mutation | Lock ownership |
| --- | --- | --- |
| `runHeadlessAttemptSurvive` after subprocess start | `saveRegistry(a)` writes PID/log snapshot | `Agent.toRecord()` snapshots under `Agent.mu`; `registryStore.Save` serializes file write |
| `processHeadlessLine` on `init`/`system` or terminal `result` | `saveRegistry(a)` refreshes captured session ID | `Agent.mu` snapshots changed fields; `registryStore.Save` serializes file write |
| `startConvoProcessSurvive` and one-shot conversational survival | `saveRegistry(a)` writes FIFO/log/PID snapshot | `Agent.mu` snapshots changed fields; `registryStore.Save` serializes file write |
| `processConvoLine` on session capture for detached interactive Claude | `saveRegistry(a)` refreshes session ID | `Agent.mu` snapshots changed fields; `registryStore.Save` serializes file write |
| `markAgentDone` | `Delete(a.ID)` removes record and FIFO | `registryStore.Delete` serializes file/FIFO removal; `Manager.mu` separately decrements `liveCount` |
| `ReattachAll` startup sweep | `List()` reads records; dead records call `Delete` | `registryStore.List/Delete` serialize file operations; each reattached agent registration uses `Manager.mu` |
| `reattachInteractive` and `reattachPerTurnConvo` dead/zombie cleanup | `Delete(r.ID)` removes stale record/FIFO | `registryStore.Delete` serializes file/FIFO removal |
| `finalizePerTurnOneShot` after recovered completion | `Delete(r.ID)` removes completed one-shot record | `registryStore.Delete` serializes file/FIFO removal |

## Refactor Boundary

The clean split is persistence-only: code outside `registry.go` should depend on
the unexported `survivalRegistry` interface (`Save`, `List`, `Delete`) rather
than the concrete store. FIFO path construction is a pure layout helper, not a
persistence capability. Reattach policy still needs
`Manager.mu` because it registers live agents and updates `liveCount`, so moving
reattach wholesale out of `Manager` would require an explicit lifecycle API for
map registration and completion callbacks.

The `runner_*.go` family remains fused. Those files share provider-specific
stream parsing and `ConvoEvent` normalization, and they only touch the registry
through `saveRegistry` or the narrow survival registry interface.
