# Release Notes

Operator-facing changes that alter what a running board does, in the order
they landed. Sybra auto-deploys `main` (see `CLAUDE.md`'s Server Deployment
section), so "release" here means "reached `main`," not a tagged version.

## 2026-09-01 — Database task documents are bounded

The database backend now caps each primary task document at **1 MiB**. Each
agent run keeps at most 2,000 bytes of prompt and 2,000 bytes of result, and a
task keeps its newest 100 run records. Full provider output remains in the
run's log file when that file is still available.

On startup, Sybra compacts existing oversized rows in the background, one row
per transaction, so the board can begin serving without first loading or
rewriting the whole task corpus. Historical run and workflow records are
discarded oldest-first; the task description is shortened only as a last
resort. Any affected task carries a durable compaction receipt and shows a
warning in task detail with the observed size and what was removed. Tasks
already within the limits are serialized exactly as before.

## 2026-08-09 — Storage backend defaults to a database

An install that has never set `database.backend` in `config.yaml` now lands
on `sqlite` the next time it starts, instead of the per-domain filesystem
stores under `SYBRA_HOME` (`~/.sybra` by default) it used before. This
happens automatically — nothing needs adding to the config file.

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
