# Standalone worker updates

The local leader selects its **clean running commit**, not an arbitrary newer
branch head. `sybra-worker-update` performs deployment mechanics on the execution
host; it starts no board, schedules no tasks, and invokes no provider. The worker
continues running while a candidate is downloaded and checked.

## Release and safety gates

The main-push `CI` workflow publishes `sybra-worker-linux-amd64` and
`sybra-worker-linux-arm64` only after its other jobs succeed. Each contains
`sybra-agentd` and the bootstrap updater, attested with GitHub's hosted-runner
identity. Verification binds the repository, `.github/workflows/ci.yml`, main
source ref, source commit, signer commit, and each binary digest. PR and merge
group runs do not publish. Artifacts expire after 90 days; a missing artifact
leaves the current worker running and requires a successful main CI rerun.

The leader's `/worker/v1/release` refuses dirty or unidentified builds. An
updater with an incompatible protocol requires an explicit operator bootstrap.
The unauthenticated `/health` identity must match before any bearer request;
redirects and non-loopback cleartext URLs are refused.

An update has a private, durable nonce and a separate SQL hold, visible as
`updateHeld` in worker diagnostics. Both start-command paths honor it; existing
commands and handback still flow. It never changes an operator's disabled or
draining state. Holds survive leader/worker restarts and do not time out.

Before restart, the updater proves its exact hold still exists, that no accepted
run remains across any session of the worker, and that the local durable spool
has no provider ownership, events, or artifacts. This local check closes the
window between terminal delivery and the next backlog heartbeat. The explicit
daemon `node_id`, leader URL, token environment name, service command, live
process command, and local spool session must match the deployment being held.

The root-owned journal records intent before activation and before releasing a
hold. The pointer changes atomically; only `sybra-agentd.service` is restarted.
A fresh healthy session must report the target build before release. An unhealthy
candidate rolls back after two minutes to the retained prior SHA, still under
the exact hold and local quiescence checks. Once hold release has been attempted,
no health timeout can trigger a rollback: its successful reply may have been
lost while the worker resumed accepting work.

## One-time enrollment

Use the standalone unit/drop-in from the
[worker-only rollout](factory-rollout.md#worker-only-rollout), with
`/opt/sybra/worker-current` pointing to a retained, known full-SHA release under
`/opt/sybra/worker-releases`. Do not enable the old `sybra.service` on an
execution-only host.

1. Upgrade the leader through its normal green-CI deployment. Wait for the
   matching main CI artifact to finish publication.
2. Download that exact successful push run's architecture artifact with `gh run
   download`. Before installing or executing either binary, verify both using
   `gh attestation verify FILE --repo Automaat/sybra --signer-workflow
   Automaat/sybra/.github/workflows/ci.yml --source-ref refs/heads/main
   --source-digest SHA --signer-digest SHA --deny-self-hosted-runners
   --hostname github.com`, replacing `SHA` with the leader's full revision.
3. Install the verified updater as root-owned mode `0755` at
   `/opt/sybra/bin/sybra-worker-update`. The updater itself is a separately
   enrolled root helper; worker activation does not silently replace it.
4. Copy `deploy/worker-update.yaml.example` to `/etc/sybra/worker-update.yaml`.
   Fill the expected board health identity and this daemon's explicit `node_id`;
   match its `leader_url` and `token_env`. Keep the config, release tree, state
   directory, current-link parent, and all ancestors root-owned and not
   group/world writable. Keep the agent config root-owned and readable by the
   service account. Do not change provider/run-state ownership.
5. Provide a root-controlled `gh` executable supporting the verification flags
   above, with read access to this public repository's Actions artifacts and
   attestations. Credentials come from the existing environment files, never
   command arguments or YAML values. The unit forces a root-controlled PATH
   and a private writable verification cache.
6. Install `deploy/systemd/sybra-worker-update.{service,timer}` and run
   `systemctl daemon-reload`, then `systemctl enable --now
   sybra-worker-update.timer`. The timer checks about once a minute. The
   service's state directory is created by systemd, mode `0700`.

Check `journalctl -u sybra-worker-update.service`, the current-link target, and
`/worker/v1/diagnostics?workerId=WORKER_ID`. That filtered endpoint returns only
live sessions, so years of replaced sessions cannot grow an updater response
without bound. Confirm the fresh build, readiness, available capacity, and no
update hold after activation; preserve any independent operator disable.

## Recovery

Busy workers wait without forced cancellation. A down leader or broken tunnel
retains the journal and hold; restoring connectivity resumes the same operation.
Never delete a hold or edit the journal to make capacity appear available.

A failed activated candidate is quarantined after healthy rollback. Repair its
cause, then explicitly run the updater with `-retry-quarantined` through the
same root environment to retry that SHA; all verification gates still apply.
A newer leader-selected SHA is considered normally. Failed preflight retains
one bounded `.stage-SHA` directory, not a fresh download on each tick; explicit
retry can redownload its two known binary files. Prior releases and failed
stages are retained for operator inspection; there is no automatic pruning or
recursive cleanup of worker data.

If rollback itself cannot be proved safe, the hold stays and the error identifies
the missing proof. Inspect the exact service/config/pointer and private journal
locally. Never paste the journal nonce, credentials, spool contents, or work-derived
events into public issues. The updater reads but never writes the daemon spool,
run registry, workspaces, or provider state.
