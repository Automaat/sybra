# Contributing

## Frontend Dependency Pin Strategy

### Caret (`^`) — public UI libraries

Use caret for icon sets, UI kits, and runtime helpers where minor/patch updates are expected to be safe and security patches are desirable:

```json
"@lucide/svelte": "^1.8.0",
"@xyflow/svelte": "^1.5.2"
```

Before merging a Renovate PR that bumps these, visually verify that icons or layout have not regressed.

### Exact pin — build tools

Pin build tools to an exact version to prevent unexpected toolchain drift:

```json
"vite": "8.0.10",
"vitest": "4.1.5",
"typescript": "6.0.3"
```

### Lock file integrity

`frontend/package-lock.json` is committed and must stay in sync with `package.json`.

- **Local:** run `mise run hooks:install` once after cloning to activate the pre-commit hook. The hook rejects commits where `package.json` is staged but the lockfile is stale.
- **CI:** the `npm Lockfile Integrity` job runs `npm install --package-lock-only` and fails the build if the lockfile drifts.

Always commit an updated `package-lock.json` when changing `package.json`.

## Version Update Process

`mise.toml` is the single source of truth for tool versions (Go, Node). When bumping a version:

1. Update the version in `mise.toml` (`[tools]` section).
2. For **Go**: also update `go.mod` (`go X.Y.Z` directive), README.md tech stack table, and CLAUDE.md.
3. For **Node**: also update the `engines.node` field in `frontend/package.json`.
4. Run `cd frontend && npm install` to regenerate `package-lock.json` if Node changed.

The `Doc Version Sync` CI job enforces that all four locations stay in sync and will fail if any are stale.
