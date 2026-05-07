# Wails v2 → v3 Migration Plan

**Status:** planning approved, Phase 1 spike in branch `feat/wails-v3-poc` (draft PR).
**Issues:** [#609](https://github.com/Automaat/sybra/issues/609) (this plan), [#319](https://github.com/Automaat/sybra/issues/319) (strategic tracker), [#314](https://github.com/Automaat/sybra/issues/314) (App-struct refactor — folded into Phase 2).
**Today:** Wails v2.12.0 (`go.mod:9`).
**Target:** Wails v3 stable. Phase 1 spike pinned to `v3.0.0-alpha.87` (2026-05-07); refresh at Phase 2 kickoff.
**Last updated:** 2026-05-07.

## Why migrate

- v2 is in long-term support, v3 is the active branch. Staying on v2 = compounding tech debt.
- v3 service pattern eliminates the `App.ctx` plumbing and per-service context wiring (see #314 + recent commit `201077f`).
- Multi-window decoupled from `application.New(...)` unlocks the agent-dashboard direction.
- Typed events replace `runtime.EventsEmit(ctx, name, any)`.

## Audit

| Surface | Count | Where |
|---|---|---|
| Wails v2 import statements | 7 | `main.go:18-24` |
| `wails.Run` call | 1 | `main.go:91` |
| Bound services | 12 + `App` | `internal/sybra/services.go:87-94` |
| Total exposed methods | ~91 | across `internal/sybra/svc_*.go` |
| `runtime.EventsEmit` call sites | 2 | `main.go:54`, `main.go:82` |
| `runtime.Quit` call sites | 3 | `main.go:70, 77, 108` |
| Internal packages using Wails runtime | 0 | (all funnels through `main.go`) |
| Frontend transport shim | 1 | `frontend/src/lib/api.ts` |
| Frontend stores using `EventsOn` | 5 | `frontend/src/stores/*.svelte.ts` |
| `cmd/sybra-cli`, `cmd/sybra-server` Wails imports | 0 | unaffected |
| CI `wails generate` step | 1 | `.github/workflows/ci.yml:92-110` |
| `wails build` in CI | 0 | desktop builds are local/release-only |

## Target architecture

- `wails.Run(&options.App{...Bind: [...]})` → `application.New(application.Options{Services: [...]})`.
- Each `internal/sybra/svc_*.go` becomes a v3 `Service`. No shared `App.ctx` field; lifecycle via `OnStartup(ctx, options)` / `OnShutdown()` per service.
- `runtime.EventsEmit(ctx, …)` → `app.Event.Emit(...)`. Listeners in Svelte: `EventsOn` → v3 typed event API.
- Bindings: `frontend/wailsjs/go/sybra/*.js` → `frontend/bindings/...`. Update import paths in `frontend/src/lib/api.ts` only (the desktop ↔ web shim absorbs the change; stores stay untouched).
- Window: `app.Window.NewWithOptions(application.WebviewWindowOptions{...})` after `application.New`.
- Menus: v2 `menu.NewMenu()` with `Cmd+W` / `Cmd+Q` handlers → v3 `app.NewMenu()` (same surface, different namespace).
- Asset embed: `&assetserver.Options{Assets: assets}` → `application.AssetOptions{Handler: application.BundledAssetFileServer(assets)}`.

## Phased plan

### Phase 1 — Spike (active, ~2 days)

- Branch `feat/wails-v3-poc`, draft PR, no merge to main.
- Add `github.com/wailsapp/wails/v3 v3.0.0-alpha.87` to `go.mod` alongside v2 (different major version paths coexist).
- New binary `cmd/sybra-v3-poc/` opens a single window, embeds a tiny test page, calls one v3 service.
- Parallel-track service `internal/sybra/v3svc/info.go` (port of `svc_info.go`) — proves the v3 service pattern end-to-end.
- **Exit criterion:** `go run ./cmd/sybra-v3-poc` opens a window on macOS, version readout renders.

### Phase 2 — Service conversion (~1 week, gated on v3 ≥ beta)

- Convert all 12 `svc_*.go` files to v3 `Service`.
- Drop `App.ctx`, `wireServices()`, and per-service context plumbing — #314 lands here.
- Migrate v3-side tests; keep v2-side tests on main untouched until cutover.
- **Exit criterion:** parity test suite passes against v3 binary.

### Phase 3 — Event migration (~2 days)

- Rewrite the 5 runtime call sites in `main.go` (`EventsEmit×2`, `Quit×3`).
- Update the 5 frontend `EventsOn` listeners in `frontend/src/stores/`.
- **Exit criterion:** quit-confirm flow, task-update broadcasts, background-ops events all observable in dev.

### Phase 4 — Bindings + frontend (~2 days)

- Regenerate bindings under `frontend/bindings/`.
- Update import paths in `frontend/src/lib/api.ts` — single file, all stores benefit.
- Verify both desktop and web builds (`mise run build:server` for the HTTP target serving `frontend/dist-web`).
- **Exit criterion:** `npm run check`, desktop `wails3 build`, and `mise run build:server` all green.

### Phase 5 — CI + cutover (~1 day)

- Update `.github/workflows/ci.yml` binding-sync check to v3 generator.
- Squash-merge the v3 branch onto main, replace `main.go` and the v2 `cmd/sybra/` entry point.
- Drop `github.com/wailsapp/wails/v2` from `go.mod`.
- Close #319 and #314.

**Total active engineering:** ~2 weeks. Calendar elapsed depends on v3 reaching beta.

## Risks

| Risk | Mitigation |
|---|---|
| v3 alpha API churn between alpha.87 and beta | Pin alpha tag in spike; re-spike at beta and refresh this doc |
| Frontend bindings regen breaks Svelte stores | All frontend usage funnels through `frontend/src/lib/api.ts`; binding-path changes localized to one file |
| Server-mode regression (`cmd/sybra-server` reuses `frontend/dist-web`) | Include `mise run build:server` smoke in Phase 4 exit criterion |
| Webkit2gtk on Linux desktop CI | Out of scope; CI never built desktop. Defer Linux smoke to manual + release workflow |
| Hidden Wails v2 surface beyond the audit | Phase 2 starts with `grep -r "wails/v2" .` — fail fast if anything unexpected surfaces |
| In-flight #314 conflicts | Decision: do not start #314 separately; it lands inside Phase 2 |

## Decision: branch-and-cutover, not in-place

v3 alpha is unstable; in-place migration would block all main-branch work for ~2 weeks. Branch-and-cutover keeps main shippable and lets the spike inform the real plan. Spike branch survives (rebased periodically) until v3 hits beta, then merges as one PR.

Recorded here in lieu of a separate ADR.

## Verification

Each phase ends with these gates green:

```bash
go build ./...
go test ./...
mise run lint                     # golangci-lint + oxlint + svelte-check
mise run build:server             # web/server binary smoke (no GTK needed)
```

Phase 1 + 4 add manual smoke:

```bash
go run ./cmd/sybra-v3-poc         # Phase 1 — window opens, version visible
wails3 dev                        # Phase 4 — full Svelte frontend over v3
```

## Out of scope

- Linux desktop build verification (CI doesn't ship one today).
- Windows builds.
- Wails v3 plugin ecosystem.
- Any orchestrator-brain changes (`orchestrator/CLAUDE.md`) — independent of the framework.

## Open questions

- v3 stable ETA: out of our hands. Phase 2 start is gated on v3 ≥ beta.
- macOS-only Phase 1 smoke. Linux desktop deferred per CLAUDE.md "Server-context quality gates".
- Refresh cadence for alpha pin: re-spike at every minor alpha bump or only at beta — recommend beta-only.
