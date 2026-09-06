# Factory reliability rollout

The local leader owns the board, scheduling, canonical worktrees and completion
evidence. Remote `sybra-agentd` processes execute runs and return durable events
and artifacts. A worker does not need a second board on its host.

The rollout preserves this topology and introduces project-scoped GitHub CI
verification. Focused local checks remain part of coding, independent agents
review and exercise behavior, and GitHub owns the full deterministic suite.

Implementation and operational acceptance are tracked in
[the factory tracker](https://github.com/Automaat/sybra/issues/3507).

## Why this change

The September 2026 investigation found three separate failure classes:

- The reverse SSH tunnel was alive but forwarded to a retired desktop port.
  Initial daemon registration exited on each connection failure, eventually
  exhausting systemd's start limit. Process liveness did not prove leader
  reachability.
- Quota/storage failures affected durable writes. A worker could remain
  reachable while unable to prepare or return a run. Artifact collection
  errors also replaced the original execution error, obscuring diagnosis.
- Shutdown waited for scheduler-owned limits pollers before canceling their
  context. This forced timeouts instead of draining cleanly.

Historical artifact errors and quota errors are distinct observations; the
investigation did not establish that every missing-checkout failure was caused
by quota. Repeated error-log records are not unique failed runs.

## Worker-only rollout

1. Inventory accepted runs on both the local leader and the old remote board.
   Disable new old-board dispatch/automations, drain accepted runs, and retain
   the old board's database, registry, logs, and worktrees. Do not delete or
   copy a live worker's state root to create another identity.
2. Build `./cmd/sybra-agentd` from the green, merged revision for the worker OS.
   Stage an immutable release under `/opt/sybra/worker-releases/<revision>/`.
   Validate it using `-check-config -config /etc/sybra/sybra-agentd.yaml` in the
   existing service environment; do not print the environment or token.
   Point `/opt/sybra/worker-current` at this release. Retain the previous target
   for rollback. Do not point it inside the full-board release-pruning tree.
3. Back up and upgrade [the base unit](../deploy/systemd/sybra-agentd.service)
   at `/etc/systemd/system/sybra-agentd.service`, preserving any host-specific
   environment files and survival settings. The new base has no board dependency.
   **An old base cannot be repaired with empty `PartOf=`/`After=` lines in a
   drop-in:** systemd dependency lists are additive and cannot be reset there.
   Install [the standalone drop-in](../deploy/systemd/sybra-agentd-standalone.conf)
   as `/etc/systemd/system/sybra-agentd.service.d/standalone.conf`; it changes only
   the worker executable/working directory. Reload systemd, inspect
   `systemctl show sybra-agentd -p PartOf -p After -p KillMode -p ExecStart`, and
   confirm neither dependency property contains `sybra.service` (including
   dependencies introduced by other host drop-ins). Keep `KillMode=process`,
   existing environment files, sandbox enforcement, and durable state paths.
   Clear an old start-limit failure with `systemctl reset-failed sybra-agentd`,
   then restart the worker. The co-hosted board's post-healthcheck refresh hook
   remains supported without a lifetime dependency, and resets a failed
   co-hosted worker's start budget once a healthy agentd-capable release exists.
4. On the laptop, install [the tunnel reconciler](../deploy/bin/sybra-leader-tunnel.sh)
   and adapt [the launchd template](../deploy/launchd/dev.sybra.agentd-tunnel.plist.example)
   with absolute paths, the actual leader home, and an SSH host alias. Replace
   the old fixed-port launch agent, rather than running two forwards on the
   same remote port. Ensure launchd's PATH contains `jq`, `curl`, `shasum`,
   `awk`, and `ssh`. The reconciler verifies `/health`'s service and canonical
   home identity before exposing the leader through SSH; HTTP 200 alone is
   not sufficient. It never forwards to a remote board as a fallback.
5. Confirm the **local leader's** `/worker/v1/diagnostics` reports the expected
   stable worker ID, current build, live lease, `readiness=ready`, and free
   capacity. Then disable/stop the drained remote `sybra.service`. Keep its
   deployment and data intact for rollback. Worker upgrades now roll the
   separately validated worker release pointer; a board autoupdate no longer
   upgrades or restarts a standalone worker.

Rollback: restore the prior worker release pointer and restart with the same
state/config. To restore the old co-hosted service topology, drain first, remove
only the standalone drop-in, reload systemd, and re-enable the old service; its
post-healthcheck hook again refreshes the worker onto the board's release.
Never run both boards' automations against the same workload during rollback.

Transient initial connection failures now retry with capped jittered backoff,
without replacing registration identity or requiring a service restart.
Permanent authentication/protocol/TLS verification errors remain visible
failures. Cancellation interrupts the registration wait.

Readiness checks free bytes and an actual temporary write/rename in both state
and workspace roots, plus sandbox availability. `min_disk_free_bytes` defaults
to 1 GiB; set a larger reserve for larger workloads. A readiness failure stops
new placement, not heartbeats or delivery of accepted work. If readiness is
lost after reservation, a never-started run is deferred with a fresh durable
admission identity and a bounded workflow wait. This consumes neither a coding
retry nor a parallel sibling's identity.

Diagnostics separate leader-unacknowledged `pendingEvents` from heartbeat
snapshots of worker `bufferedEvents`, `pendingArtifacts`, and
`oldestBufferedEventSeconds`. Check lease freshness before interpreting those
snapshots. Restore delivery or drain on growing backlog; never clear the spool
to make an alert disappear. Execution and artifact errors remain separate.

## CI ownership and pilot

Enable `checks.ci.enabled` per project with an explicit `required_checks` list
from its trusted default branch. The Sybra repo is the first pilot. An
unreadable policy is an error, not an implicit opt-out.

The leader pushes an **unlinked draft** after implementation/codegen. GitHub
starts full verification while focused local checks, independent review, and
black-box testing proceed. The draft stays out of PR-monitor ownership until
the normal PR tail validates completion evidence and publishes/links it.
Branch lookup makes this handoff recoverable after a restart. All OPEN lookup
and creation-conflict paths preserve the same ownership rule.

Review reuse requires a clean result on the exact source commit and task-input
digest (project, title/body, plan/contract, manual-test instructions). Inputs
are persisted before paid dispatch. Changed inputs invalidate reuse; a fix
does not relabel an old review with the new SHA. CI-enabled projects require
fresh evidence even if the legacy global evidence toggle is off. Existing
explicit no-review/trivial policies still determine applicable local criteria.

The existing PR monitor owns asynchronous waits, CI repair, and protected
merging—there is no second CI polling workflow. Normal post-publication repair
pushes retain their existing PR-fix verification tail; they do not incorrectly
re-enter the initial draft-publication gate. Existing task/kind/head/fingerprint
deduplication remains the single fix-dispatch owner.

Required checks must all actually succeed on the PR head observed in the same
uncached snapshot. Missing, pending, skipped, neutral, failed, and mismatched
revision evidence cannot approve. The merge command pins that head. Native
auto-merge is not armed for CI-enabled projects because it could outlive the
verified revision and the custom list need not equal GitHub branch protection.
The normal protected merge/queue path remains mandatory.

Full lint/race/e2e/coverage/security/build jobs remain in GitHub Actions for PRs
and `merge_group`. Only superseded **PR** runs are canceled; main and merge-group
runs are independent. Wails drift fails with a downloadable patch instead of
pushing an unreviewed commit or creating token-recursion check stalls.

Local work keeps formatting, whitespace/isolation checks, changed-package
regressions, independent code review, and real behavior testing. `mise run
verify` remains an optional comprehensive diagnostic; it is not repeated at
every commit/stage. SQL changes still warrant explicit dual-engine tests.

Pilot the first 20–30 completed changes before expanding to other projects.
Compare median/p90 coding-to-merge time, first-pass CI rate, review/fix starts
per head, infrastructure deferrals, handback age, and shutdown duration. Count
actual accepted provider runs separately from zero-cost infrastructure waits.
Rollback CI policy by explicitly disabling the pilot and restoring the full
local verification list together; do not disable CI/branch protection.

Acceptance includes reconnect after leader port changes, wrong-board rejection,
registration cancellation, restart/duplicate refusal with parallel siblings,
storage-pressure recovery, exact-head CI rejection, unchanged-contract review
reuse, and preservation of verifier input across route-publication failures.
