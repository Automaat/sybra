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
| Build concurrency | 5 | Keeps a useful batch in flight |
| Maximum group size | 5 | Five independent PRs share one combined CI run |
| Minimum group size | 1 | A single ready PR is never stranded |
| Minimum wait | 5 minutes | Gives concurrently-ready PRs a short batching window |
| Status-check timeout | 45 minutes | Exceeds the longest required job timeout |
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
2. Queue two small independent pull requests and confirm GitHub creates one
   merge group containing both.
3. Confirm all required checks run against the merge-group commit before either
   pull request reaches `main`.
4. Queue two pull requests whose combination fails a required check and confirm
   neither is merged as a successful group.
