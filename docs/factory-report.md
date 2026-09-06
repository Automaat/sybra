# Factory latency report

Run `sybra-cli --json factory --since 7d` against the local leader. To compare releases, select a full Sybra revision with `--release <sha>` and explicit UTC/RFC3339 `--since` / `--until` boundaries. Dates mean midnight UTC; the end is exclusive. `unknown` and `mixed` are valid release filters. The server exposes the same aggregate through `AuditService.GetFactoryReport`.

The window is at most 31 days, 250,000 events, and 32 MiB of matching input; excessive input or unreadable records are refused, never silently sampled. Limits apply inside both storage readers, before retaining the full input. Output contains four fixed phases, bounded release counts, and no task/project IDs, repository names, agent transcripts, or task content. It reuses the audit store and canonical run lifecycle/accounting normalization. It does not query GitHub or spawn an agent to produce a report.

| Phase | Measurement |
| --- | --- |
| queue | Observed admission-queue entry to dequeue; restored intent is one interval. Removal/reconciliation without dequeue is censored. |
| agent | First observed start to canonical terminal event for one agent identity. Resume/compatibility events do not create additional runs. |
| ci | First observed blocking CI gate to verification by the existing PR monitor, keyed by task/head using an opaque digest. This is not GitHub job duration; early CI can overlap review/testing. |
| deploy | Auto-update candidate first seen to that target leader build's startup boundary. This includes approval/coalescing/build wait. Standalone worker activation coverage is explicitly unavailable here. |

Median and nearest-rank p95 include only complete intervals. Every phase exposes sample count, open intervals, unknown starts, censored intervals, and availability. Null percentiles mean insufficient coverage, never zero latency. An open interval only means that the audit window has no end; it is not a live-process assertion. Missing historical boundaries are not invented or filled from current configuration.

New live audit writes are stamped with the running leader's clean build revision, not checkout HEAD. Imports happen before stamping is installed, and historical reads remain unchanged. Unknown/dev/dirty builds stay unknown. Intervals crossing known leader builds are `mixed`; a missing endpoint revision keeps them unknown. Deployment intervals are attributed to the target release. Release event counts describe the whole selected time window, including when a release filter narrows phase/run aggregates.

Completed work counts unique tasks whose final observed status is done, so duplicate completions and reopen/recomplete cycles do not inflate throughput. Reopened tasks are also counted uniquely. Retries mean an observed new run for the same task/role after a failed terminal run; they do not include resumes under the same agent ID. Costs are observed terminal-run spend inside the selected window (and release filter), not lifetime task costs. Missing costs are counted explicitly rather than assumed free. Completed-task window cost is the subset belonging to the unique completed cohort.

For rollout comparisons, use equal windows and inspect sample/unknown/open counts before claiming improvement. Earlier seven-day averages spanning multiple incident and release regimes are a baseline mixture, not post-fix performance. This report changes no routing, concurrency, provider budget, CI gate, or deployment policy.
