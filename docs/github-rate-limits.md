# GitHub Rate Limits

Sybra drives GitHub entirely through the `gh` CLI (`internal/github`). All calls
funnel through a single request gate (`ghGate`) that paces requests (150ms
spacing), backs off on `Retry-After` / rate-limit responses, and now tracks the
live budget for every resource bucket.

This doc covers the knobs that control GitHub request volume and how to lift the
ceiling with a GitHub App.

## Where the requests come from

The volume drivers are the **periodic GraphQL search polls**, not per-PR REST
calls:

| Poller | What it searches | Default cadence |
|--------|------------------|-----------------|
| reviews | `author:@me`, `review-requested:@me`, `reviewed-by:@me` (3 legs) | fast 2m / slow 10m |
| issues  | assigned + labeled (`sybra`) issues, combined in one request | 10m |
| renovate | Renovate-bot PRs across project repos | fast 2m / slow 10m |

Per-PR REST reads (`gh pr view`, `/pulls/{n}/reviews`) are bounded and short-TTL
cached; they are not the bottleneck.

## Tuning knobs (`github:` config block)

```yaml
github:
  enabled: true

  # Split polling across machines that share one token. "secondary" skips the
  # reviews/issues/renovate search polls so only the primary spends the shared
  # budget. Empty/"primary" runs them. Triage and on-demand per-PR calls run
  # everywhere regardless.
  poller_role: primary

  # Poll-interval overrides in seconds (0 = built-in default). Raise these on a
  # low-limit (personal token) instance; lower them on a high-limit App-token
  # instance.
  reviews_fast_seconds: 120      # default 120 (was 60)
  reviews_slow_seconds: 600      # default 600 (was 300)
  issues_seconds: 600            # default 600 (was 300)
  renovate_fast_seconds: 120     # default 120 (was 60)
  renovate_slow_seconds: 600     # default 600 (was 300)
```

On top of the configured interval, every poller is stretched automatically when
the shared budget runs low (`github.ScaleInterval` → up to 4x as the lowest
resource bucket approaches exhaustion). The budget is refreshed every 60s from
the **free** `GET /rate_limit` endpoint, so the throttle and the gate see
accurate remaining quota for every `gh` path — including the `gh pr view` /
`gh issue` subcommands whose response headers the gate can't observe directly.

### Running two machines on one token

Set the laptop to `poller_role: secondary` (or split by `project_types`) so the
server is the sole search poller. Without this, both instances run the same
`@me` searches and double-bill the shared token — the fastest way to trip the
**secondary** rate limit.

## Lifting the ceiling: GitHub App auth

A personal token gets 5,000 REST req/hr. A GitHub App **installation token**
gets 15,000 REST req/hr (and higher GraphQL headroom). Sybra mints a short-lived
installation token, refreshes it every 30m, and injects it into the `gh`
subprocess via `GH_TOKEN` — no call site changes.

```yaml
github:
  app:
    enabled: true
    app_id: 123456
    installation_id: 7891011
    private_key_path: /data/sybra/github-app.pem
```

When unset/disabled, `gh` uses its own auth unchanged.

### Manual setup (one-time, done in the GitHub UI)

1. **Create the App** — Settings → Developer settings → GitHub Apps → *New
   GitHub App*. Give it a name; homepage URL can be anything.
2. **Permissions** — Repository: *Contents* (read/write), *Issues* (read/write),
   *Pull requests* (read/write), *Metadata* (read). Organization/Account: none.
   Match these to what your automations actually do.
3. **Webhook** — uncheck *Active* (Sybra polls; it doesn't receive webhooks).
4. **Create**, then note the **App ID** (shown on the App's page).
5. **Generate a private key** — on the App's page, *Private keys → Generate a
   private key*. A `.pem` downloads. Put it where the server can read it
   (e.g. `/data/sybra/github-app.pem`, mode `0600`) and set `private_key_path`.
6. **Install the App** — *Install App* → choose the account/org → select the
   repositories Sybra manages.
7. **Get the installation ID** — after install, the URL is
   `https://github.com/settings/installations/<INSTALLATION_ID>`. Use that
   number for `installation_id`.
8. Set the `github.app` block above and restart Sybra. Startup logs
   `github.app.enabled`; a bad key/permission logs `github.app.disabled` and
   falls back to gh's own auth (never fatal).

The private key never appears in config — only its path is stored. For
work-typed projects, keep the key off any artifact that could reach a public
issue/PR (see Work-Data Confidentiality in `CLAUDE.md`).

## Native auto-merge (`github.native_auto_merge`)

```yaml
github:
  native_auto_merge: true  # default false
```

An accelerator on top of Sybra's own green-gated merge (`internal/github.MergePR`),
not a replacement — when unsupported or disabled the legacy merge stays the
fallback. When on, and once Sybra's own review/fix cycle for a pet-project PR is
done (Copilot reviewed, threads resolved, no changes-requested, mergeable), Sybra
arms GitHub's native `gh pr merge --auto --squash` instead of polling for CI
itself. GitHub then merges the PR the moment its required checks go green,
handled entirely on GitHub's side over REST — cheaper than Sybra's own
GraphQL-backed poll loop for the last CI-green mile.

**Pre-flight capability check** (`internal/github.SupportsNativeAutoMerge`,
short-TTL cached ~3m): before arming, Sybra confirms the repo has
`allow_auto_merge` on *and* the PR's **base** branch's protection rules require
both status checks and conversation resolution. Fails closed — a repo without
auto-merge enabled, a base branch with no protection configured (a common,
valid state, not an error), or protection missing either required check all
result in "unsupported," so Sybra falls back to its own merge gate.

**Armed PRs stay monitored.** Arming does not remove a PR from Sybra's GraphQL
(`FetchReviews`/`FetchPRForMonitor`) or REST (`fetchPRForMonitorViaREST`)
observation — the poll loop keeps watching until the PR reaches a terminal
MERGED/CLOSED state, same as any other tracked PR. The only change is that an
already-armed, otherwise-quiet PR no longer pins the fast poll interval
(`prNeedsAttention`); a newly-failing check, new review feedback, or auto-merge
getting disabled out from under Sybra all still trigger the fast cadence again.

**Disarm at scale.** If you need to bulk-disable native auto-merge on a repo
(e.g. rolling back the flag), list and clear armed PRs directly with `gh`:

```bash
gh pr list --repo <owner/repo> --json number,autoMergeRequest \
  --jq '.[] | select(.autoMergeRequest != null) | .number' \
  | xargs -n1 -I{} gh pr merge {} --repo <owner/repo> --disable-auto
```

## What was deliberately *not* done

- **Combining the 3 reviews search legs into one request.** The combined form
  (multiple aliased `search` connections in one query) times out at GitHub's
  edge for accounts with many open review requests — it was split for that
  reason (see `internal/github/review.go`). The reviews rate reduction comes
  from longer intervals + the budget throttle + App-token headroom instead.
- **A go-github/githubv4 client with ETag conditional requests.** ETag 304s are
  free only against the REST budget, but Sybra's load is GraphQL-search-dominated
  (POST, which can't use conditional requests). The live-rate-header limiter half
  of that idea is already covered by the `/rate_limit` refresher + gate.
