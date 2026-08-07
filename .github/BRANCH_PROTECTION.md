# Main branch merge protection

`main` uses GitHub's merge queue to prevent independently-green pull requests
from breaking the branch when their changes interact. The queue tests a
synthetic commit containing the proposed group, so the check result covers the
same combined tree that reaches `main`.

The `CI` workflow must keep its `merge_group: checks_requested` trigger. Every
required check is supplied by that workflow. Removing the trigger leaves merge
groups waiting forever rather than safely falling back to pull-request checks.

## Repository settings

The repository ruleset named `protect-main` targets `refs/heads/main` and has a
merge queue rule with these values:

| Setting | Value | Reason |
| --- | --- | --- |
| Merge method | Squash | Matches Sybra's pull-request merge policy |
| Build concurrency | 5 | Runs up to five speculative CI builds concurrently instead of serially |
| Maximum merge group size | 5 | Merges at most five successful queue entries at once |
| Minimum merge group size | 1 | A single successful queue entry is never stranded |
| Minimum wait | 0 minutes | Does not delay a ready entry waiting for a larger merge group |
| Status-check timeout | 45 minutes | Allows normal required-check duration while bounding stalled CI |
| Grouping strategy | Head green | The group's combined head must pass every required check |

Keep the classic `main` branch protection's required status checks enabled.
The queue rule controls how changes enter the branch; the status-check list
controls what the synthetic merge-group commit must pass.

Do not replace the queue with only "Require branches to be up to date." That
setting prevents a stale PR from merging, but serializes independent PRs behind
a fresh full CI run after every preceding merge.

## Verification after a settings change

1. Confirm `.github/workflows/ci.yml` still handles both `pull_request` and
   `merge_group` events.
2. Queue two small independent pull requests and confirm their speculative
   merge-group builds can run concurrently.
3. Confirm the later synthetic branch contains the current base plus both
   queued changes and passes all required checks before the later pull request
   reaches `main`.
4. Queue two pull requests whose combination fails a required check and confirm
   the later pull request cannot merge behind the first until its cumulative
   synthetic branch passes.
