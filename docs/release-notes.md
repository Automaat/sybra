# Release Notes

Operator-facing changes that alter what a running board does, in the order
they landed. Sybra auto-deploys `main` (see `CLAUDE.md`'s Server Deployment
section), so "release" here means "reached `main`," not a tagged version.

## 2026-08-09 — Storage backend defaults to a database

An install that has never set `database.backend` in `config.yaml` now lands
on `sqlite` the next time it starts, instead of the per-domain filesystem
stores under `~/.sybra` it used before. This happens automatically — nothing
needs adding to the config file.

- **The original files are left in place.** Each data domain's one-time
  import (`internal/dbimport`) copies rows out of the existing files and
  never moves, deletes, or writes back to them, so they stay on disk exactly
  as they were at the moment of the switch.
- **To go back**, set `database.backend: file` in `config.yaml` and restart.
  This is not a full rollback: the files are a frozen snapshot from the
  moment of the switch, so anything created, edited, or deleted on the
  database afterward has no effect on them — an edit reverts to its
  pre-switch value, a deletion reappears as a live record, and a creation
  has nothing to revert to and disappears entirely. Only restore the files
  this way if nothing changed since the migration. Switching back to a
  database afterward does not re-import either: the one-time import already
  ran and its marker persists, so anything changed while on `file` stays
  invisible to the database with no error or log line.
- **Dropping a record into a storage directory (`projects/`, `workflows/`,
  …) no longer registers it.** Those directories are the store only under
  the `file` backend; a database-backed board reads rows, and the one-time
  import has already run by the time a file lands there, so it is silently
  invisible — no error, no log line. Register state through the API instead
  (for example the Projects tab for a project). `tasks/` is not affected yet:
  a task file dropped in after startup is still picked up live, on either
  backend.

See `CLAUDE.md`'s Durable Storage Backend section for the full backend
reference, and `docs/CONFIG.md` for every `database.*` key.
