# Recovering remote result acknowledgements

The local leader owns canonical tasks, results, costs, and workflow decisions.
The remote `sybra-agentd` only executes runs and delivers events/artifacts.

There are two separate acknowledgements:

- `AppendEvents` confirms that the leader durably stored a worker's transport
  events. This lets the worker release its spool entries.
- Completion acknowledgement confirms that the leader atomically stored the
  exact terminal event's receipt with the canonical run result and cost. It
  does **not** mean all subsequent workflow side effects finished.

A file-store lock can defer the canonical write beyond the relay callback.
Completion acknowledgement now waits for that write, including its retry.
If the leader stops between the write and acknowledgement, the operator can
finish the acknowledgement without re-running the agent or completion handler.

## Operator command

Run against the local leader, not the execution worker:

```sh
sybra-cli --json cluster reconcile-results
sybra-cli --json cluster reconcile-results --apply
```

The default is a read-only dry run. `--apply` rechecks current canonical proof
and the locked terminal row; it is not an unconditional acknowledgement of
the earlier dry run. Both modes examine at most 100 results. Use `--limit 1`
through `--limit 100` to choose a smaller page. If the JSON report has
`nextAfter`, pass it as `--after CURSOR` (and repeat `--apply` for an apply
page). Restart from the first page on a later sweep to catch newer run IDs.

Reports contain counts, fixed reason names, and an opaque run cursor only;
they omit task/project identities, prompts, provider output, and errors.
The API is local-only and rejects forwarded or sandbox-marked callers.
There is no direct-file fallback when the leader cannot be reached.

## What is eligible

The immutable terminal event must match a receipt stored in the stopped
canonical run. That receipt binds the run ID, sequence, and complete event
payload. A ready artifact must already be imported or rejected. An explicit
failed artifact disposition is settled; a pending one is not. Successful
transport delivery, a terminal row, task history, or an imported artifact
alone is never sufficient proof.

Missing tasks, compacted run records, malformed terminals, absent/mismatched
receipts, and unresolved artifacts are preserved. Observer timeouts or failed
artifact-status reads cannot mint proof that later handback can validate.
`--apply` neither imports artifacts nor deletes event rows, changes tasks,
recharges costs, advances workflow steps, or starts providers. Repeated and
concurrent applies acknowledge each proven result at most once, including
after its worker lease expires or its session is replaced.

## Rollout and older results

Upgrade the leader first, then the independently managed worker binary.
New workers explicitly mark pre-admission refusals as having no deliverable
artifact using the existing failed artifact disposition. This requires no
protocol-version change. The legacy HTTP event-ACK endpoint returns 410:
shipped worker clients use `AppendEvents`, not that endpoint.

Older canonical runs have no receipt. Older pre-admission refusal events may
also omit the artifact disposition. They deliberately remain unacknowledged;
this command does not guess, rewrite immutable events, or manufacture proof
from historical status. Existing stores need no schema migration, and all
original evidence stays available for a separate investigation.

Do not start the remote board service to deploy this change in a worker-only
topology. Drain/check the execution worker before its independent upgrade,
preserve its state/spool and last-good binary, then verify readiness against
the same local leader. Begin production verification with a dry run.
