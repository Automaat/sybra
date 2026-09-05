# Release Notes

Operator-facing changes that alter what a running board does, in the order
they landed. Sybra auto-deploys `main` (see `CLAUDE.md`'s Server Deployment
section), so "release" here means "reached `main`," not a tagged version.

## 2026-09-02 — SQLite writers queue without starving readers

SQLite now admits one process-local writer at a time before that writer takes
a pooled connection. Concurrent task, workflow, audit, and tool-ledger writes
therefore wait in a context-cancellable lane while the remaining WAL
connections continue serving board reads. The pool default stays at four;
raising it does not increase SQLite write throughput.

Task-history trimming now uses a `(task_id, id)` index and the background
startup sweep enforces both the row cap and the **2 MiB byte cap** across quiet
tasks, not only tasks written again after an upgrade. The sweep is batched and
counts UTF-8 bytes consistently on SQLite and Postgres. Databases created with
incremental auto-vacuum return the pages freed by that sweep immediately.

The Prometheus endpoint now exports database pool occupancy/wait totals,
transaction latency/result, and SQLite writer-admission wait time. These
separate slow SQL from time spent waiting to begin it.

An older SQLite file created without incremental auto-vacuum cannot be
converted safely while Sybra is serving it. To reclaim its existing freelist,
stop Sybra, make a backup of `sybra.db` together with any `-wal`/`-shm` files,
then run this explicit maintenance operation against the stopped database:

```bash
sqlite3 /absolute/path/to/sybra.db \
  'PRAGMA auto_vacuum=INCREMENTAL; VACUUM;'
```

`VACUUM` rewrites the whole database and needs temporary free space near the
database's current size. It is intentionally never run automatically.

## 2026-09-01 — SQLite write-ahead logs are bounded

SQLite boards now retain at most **16 MiB** of reusable write-ahead-log space
as the next WAL generation begins after a checkpoint reset. A transaction may
still grow the WAL beyond that size while it is being committed; the cap
governs the size the file returns to and does not reject large writes.
Automatic checkpoints remain enabled at SQLite's 1,000-page threshold on every
pooled connection; a custom DSN cannot disable them or raise the retained-size
cap, because either override would make the bound ineffective.

Every database open also requests a safe truncating checkpoint. Existing
oversized WAL files therefore return their unused disk space on the next
restart without an operator stopping the service or editing database files.
The database remains in WAL mode, and committed transactions are checkpointed
into the main database before any truncation.

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
