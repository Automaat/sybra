# Independent reviewer continuity

An interrupted review may resume its own bounded provisional checkpoint. This
does not change final verdict schemas, review retries, turn ceilings, evidence
gates, or the separation between implementation and verification.

The bundled simple and staff pre-PR reviews opt in with
`review_progress_base: refs/remotes/origin/main`. Custom review steps may opt in
only with their actual full comparison ref. Reviews without an explicit base
(including inbound PR review workflows) remain stateless.

The reviewer emits separate assistant messages with this packet:

```text
<sybra-review-progress>{"inspected":["areas checked"],"findings":["provisional concerns"],"remaining":["checks still needed"]}</sybra-review-progress>
```

Each array has at most 24 items, each item at most 512 UTF-8 bytes, and the
JSON is capped at 12 KiB. The host takes the last valid packet at interruption,
before releasing the disposable verification lease. It stores a private 0600
record under the verification root's `review-progress/` directory, outside the
canonical source, verifier clone, scratch home, and final sidecars. It is not
uploaded, committed, or exposed by an artifact API. Work-derived content stays
local; any future task/public export must use the existing work scrub boundary.

Reuse requires the same task, review role, workflow execution and step,
workflow definition, task/plan/test contract, exact source commit and exact
comparison commit. No-op clean retries preserve continuity; authoritative
source/ref-history tamper checks still run for each attempt. A changed or
unavailable input prevents reuse. Corrupt or oversized cache records start a
fresh review. A successful completed attempt retires its provisional contents.
Older attempts cannot overwrite a newer checkpoint. Cache is advisory only:
removing these private records loses continuity, never final evidence.

The leader resolves the comparison commit from the canonical repository and
pins it in the disposable clone. Remote execution carries the same exact
comparison input, shipping its Git objects through the existing bounded base
bundle when needed. Placement requires the worker's `review_progress`
capability, so old workers cannot silently ignore it. The worker checks HEAD,
comparison ref, and clean source contents before collecting handback and reports
that proof in its terminal event. Missing proof or mutations discard progress,
without weakening the existing disposable-workspace/final-evidence rules.

Remote review also requires the existing `verifier_auth` capability and
restricted GitHub App credentials. The standard standalone daemon does not yet
wire that credential source or advertise that capability; normal placement
therefore keeps those reviews on the leader. Checkpoint support does not bypass
that prerequisite or enable ambient credentials on a worker.

The resume prompt contains one snapshot, never earlier prompts recursively.
It does not read implementation `NOTES.md`, seed working memory for verifiers,
or create the required final review artifact. Checkpoint packets (even malformed
ones) are excluded from interrupted-review transcript salvage.
