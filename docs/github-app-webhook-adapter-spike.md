# Spike: maintained GitHub App auth / webhook libraries (#3209)

Evaluates `github.com/bradleyfalzon/ghinstallation/v2` and
`github.com/google/go-github` as replacements for the hand-rolled GitHub App
JWT/installation-token auth in `internal/github/appauth.go` and the
`issue_comment` webhook decoding in `cmd/sybra-server/webhook_github.go`.

The adapter lives at `internal/github/ghadapter/` (`token_source.go`,
`webhook_decode.go`, with tests). It is **not wired into any dispatch path** —
this is an isolated spike, per the task's fifth step ("decide from the
results"), not a merge candidate.

## App auth: `ghadapter.TokenSource` wraps `ghinstallation.Transport`

| Safety invariant `appTokenSource` provides | `ghinstallation.Transport` alone | With `ghadapter.TokenSource` |
|---|---|---|
| Nonblocking cached-token read for `GHEnv()` | No — `Token(ctx)` mints synchronously under lock whenever the cached token is past its refresh window. Calling it directly from `GHEnv()` would let a token mint stall the `gh` subprocess request gate. | Yes, but only by keeping a token snapshot **outside** the library: `Cached()` reads a `(token, refreshAt)` pair recorded after each mint and never calls into `Transport` at all. Covered by `TestCached_EmptyBeforeFirstRefresh` and `TestCached_NeverTouchesTransportDuringConcurrentRefresh`. |
| Concurrent refresh collapse (N callers → 1 HTTP mint) | Yes, incidentally — `Token()`'s internal mutex serializes callers, and the second-in-line observes a cache hit once the first finishes. | Inherited from the library. Covered by `TestRefresh_ConcurrentCollapsesToOneMint`. |
| `ForceRefreshAppToken` — always mint, even if cache looks fresh (#2453, 401 recovery at the hourly rotation boundary) | **No such primitive.** The only refresh trigger is `isExpired()`; there is no "invalidate" or "force" method on `Transport`. | Re-implemented on top: `ForceRefresh` discards the `Transport` and builds a new one (cheap — RSA key parse, no I/O), with its own channel-based singleflight duplicating `appTokenSource.refresh`'s pattern almost line for line. Covered by `TestForceRefresh_MintsEvenWhenCachedIsFresh` and `TestForceRefresh_ConcurrentCollapsesToOneMint`. |
| App-bot identity (`<slug>[bot]`) via `GET /app`, cached for process lifetime | Not exposed — `Transport` only handles installation tokens. Needs a separate `ghinstallation.AppsTransport` + a `go-github` client. | `AppLogin()` builds that JWT-authed client and caches the slug. Covered by `TestAppLogin_FetchesAndCachesSlug`. |
| 401-triggered recovery observable by the caller | N/A (library concern) | `ForceRefresh` surfaces the mint error as-is; `Test401Recovery_ForceRefreshSurfacesThenHeals` drives a fake server through fail → heal and confirms both the error and the eventual success. |
| Ambient-auth fallback (App auth disabled → `gh`'s own auth) | N/A — the library has no "disabled" concept; a caller either constructs a `Transport` or doesn't. | Same as today: this stays a caller-side nil check (`appSource == nil` in `appauth.go`), the library gives no help either way. |

Gating `Cached()` on `Transport.Expiry()` instead — the obvious first cut —
does **not** hold the invariant, and this is the main non-obvious cost of the
library here:

- `Expiry()` is not a safe substitute for a cache-hit check. The token can
  cross `refreshAt` between the check and the `Token()` call it guards, and
  `Token()` then mints synchronously on the path that promised no I/O.
- `Expiry()` reads `Transport`'s cached token *without* the library's internal
  mutex, which `Token()` holds while writing it — so an `Expiry()` call from
  an unsynchronized reader races every concurrent mint (`go test -race`
  reports it).

So the adapter serializes all `Transport` access (`Token` + `Expiry` + the
force-refresh swap) under one mutex and publishes the result to a separate
snapshot guarded by an `RWMutex` held only for the copy. `Cached()` reads that
snapshot alone. That is the same amount of concurrency bookkeeping
`appTokenSource` already carries — the library replaces the mint, not the
caching contract around it.

**Finding:** the mint mechanics (JWT signing, installation-token HTTP call,
expiry parsing) are exactly what the library removes — about 120 lines of
`appauth.go` (`signJWT`, `mintInstallationToken`, `loadPrivateKey`'s PKCS1/8
handling). But every safety behavior the task called out as load-bearing
(nonblocking `GHEnv()`, collapsed force-refresh, 401 recovery) has to be
hand-built on top regardless, at a similar line count to what it replaces.
Net complexity reduction is real but modest, not "delete the module."

## Webhook decoding: `ghadapter.DecodeIssueComment` wraps `go-github`'s `ValidatePayload` + `ParseWebHook`

Covered by fixture, bad-signature, tampered-body, oversized-payload,
wrong-event-type, missing-Content-Type, and blank-secret tests.

Differences from `cmd/sybra-server/webhook_github.go`'s hand-rolled
`githubIssueCommentPayload` + `validWebhookSignature`:

- `ValidatePayload` requires a recognized `Content-Type`
  (`application/json` or form-urlencoded) before it will even check the
  signature; the current handler never inspects `Content-Type`.
- `go-github` enforces its own 25MB payload ceiling inside `ValidatePayload`,
  independent of (and in addition to) the
  `http.MaxBytesReader(w, r.Body, httpapi.MaxRequestBody)` the handler
  already applies at the transport layer.
- `IssueCommentEvent` decodes GitHub's full upstream schema — strictly more
  fields than the current narrow payload struct, which is more future-proof
  for new consumers but a larger trust surface per payload than the
  currently-used fields.
- The signature check itself (`ValidateSignature`) is equivalent HMAC-SHA256
  comparison; no behavioral gap there.
- **One real regression to guard against:** `ValidatePayload` skips signature
  validation entirely when the secret is empty, so a misconfigured (unset)
  `webhook.secret` would make it accept *unsigned* payloads. The current
  handler fails closed on that (`strings.TrimSpace(cfg.Webhook.Secret) == ""`
  → 401 before the signature is even considered).
  `DecodeIssueComment` therefore rejects a blank/whitespace secret before
  calling `ValidatePayload`
  (`TestDecodeIssueComment_BlankSecretRejected`); any adoption must keep that
  check at the call site rather than assume the library fails closed.

**Finding:** webhook decoding is the cleaner adoption candidate — it has no
Sybra-specific safety behavior riding on it (unlike App auth's caching/
singleflight contracts), and it deletes real code (the payload struct plus
the signature-check helper) for a well-tested upstream decoder.

## Decision

- **Webhook decoding: adopt.** Swap
  `cmd/sybra-server/webhook_github.go`'s payload struct and
  `validWebhookSignature` for `go-github`'s `ValidatePayload` +
  `ParseWebHook`, using `ghadapter.DecodeIssueComment` as the reference
  implementation. Two things must carry over, neither of which the library
  does for you: the blank-secret rejection (see above — without it an unset
  secret silently disables signature checking), and a confirmation that
  `Content-Type: application/json` is always present on GitHub's webhook
  deliveries (it is, per GitHub's docs) before relying on the new
  Content-Type check to reject anything real traffic currently accepts.
- **App auth: retain the local implementation.** `internal/github/appauth.go`
  keeps its hand-rolled `appTokenSource`. The safety invariants that matter
  here (nonblocking `GHEnv()`, collapsed force-refresh, 401 recovery) are not
  provided by `ghinstallation` and would need to be re-implemented on top of
  it at roughly the cost of what's being replaced, for a net win that's
  mostly "stop hand-signing JWTs" — real, but not enough to justify a new
  dependency plus an adapter layer carrying the exact same amount of
  Sybra-specific concurrency code it has today. Revisit if `ghinstallation`
  ever adds a first-class force-refresh/invalidate primitive.

## Test approach used

`internal/github/ghadapter/token_source_test.go` and
`webhook_decode_test.go` run the concurrent-refresh, forced-401,
token-rotation, oversized-payload, bad-signature, and `issue_comment`
fixture cases called for in the task, against the adapter — not the existing
`appauth_test.go`/webhook suites, since the adapter is a standalone spike and
was never wired in to run against those.
