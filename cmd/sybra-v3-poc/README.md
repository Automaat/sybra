# sybra-v3-poc

Phase 1 spike for the Wails v2 → v3 migration. See
[`docs/migrations/wails-v3.md`](../../docs/migrations/wails-v3.md).

**Not shipped. Not on main.** This binary lives on `feat/wails-v3-poc` only.

## What it proves

- `github.com/wailsapp/wails/v3 v3.0.0-alpha.87` compiles alongside v2 in
  the same module.
- `application.New(application.Options{Services: ...})` + a single window
  via `app.Window.NewWithOptions(...)` boots successfully on macOS.
- The v3 service pattern (`application.NewService(v3svc.NewInfoService())`)
  exposes the parallel-track `internal/sybra/v3svc.InfoService.GetVersion`
  to the frontend.
- The frontend can call the service via `/wails/runtime.js` `Call.ByName`
  with the fully-qualified method name — no generated bindings needed for
  the spike.

## Run

```bash
mise run dev:v3
# or, equivalently:
go run ./cmd/sybra-v3-poc
```

A 640×360 window opens. The page should render:

> OK — server version: `<commit-sha-or-dev>`

`mise run dev` (without the `:v3` suffix) still launches the **v2** app
via `wails dev` — that is the production code path on `main` and stays
the default until Phase 5 cutover. There is no `wails3 dev`-style hot
reload for the spike: the POC has no Vite frontend (just a static HTML
file), so a plain `go run` is the right tool. Hot reload comes back in
Phase 4 once the real Svelte frontend is wired to v3 bindings.

## Caveats

- macOS-only smoke. Linux desktop needs webkit2gtk; out of scope per
  CLAUDE.md "Server-context quality gates".
- Alpha API — pin (`v3.0.0-alpha.87`) refreshed at Phase 2 kickoff.
- Bindings generation (`wails3 generate bindings`) is deferred to
  Phase 4. The spike calls the service via `Call.ByName` to keep moving.

## Layout

```
cmd/sybra-v3-poc/
  main.go           # application.New + Window + service registration
  dist/
    index.html      # minimal page that calls the service
  README.md         # this file

internal/sybra/v3svc/
  info.go           # v3 port of svc_info.go (parallel-track, not v2)
```
