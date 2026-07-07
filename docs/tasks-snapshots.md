# Task Snapshots (Recovery Runbook)

Sybra versions the tasks dir (`~/.sybra/tasks` by default) into a dedicated
git repository at `~/.sybra/tasks-snapshots.git`
(`config.TaskSnapshotGitDir()`). A background goroutine
(`internal/tasksnapshot.Snapshotter`) polls on a fixed interval — default
30s, `task_snapshot.interval_seconds` in config — and commits any change to
the tasks dir, including files removed by a raw `rm` that bypasses the task
store's own trash-based soft delete entirely (`git add -A` is its own change
detector). A commit is also forced immediately before the trash-prune sweep
(both the boot-time pass and the periodic loop), so a snapshot always exists
right before that bulk-delete operation.

This is the recovery path the 2026-07-06 board wipe (#1576) didn't have:
that incident required reconstructing lost tasks from audit-log forensics.
With snapshotting on, recovery is a `git checkout`.

## Configuration

```yaml
task_snapshot:
  enabled: true            # default true; set false to disable entirely
  interval_seconds: 30     # default 30; 0 or negative falls back to the default
```

See `docs/CONFIG.md` for the full field reference.

## Listing history

```bash
sybra-cli tasks-history                 # last 20 commits, human-readable
sybra-cli tasks-history --limit 100
sybra-cli --json tasks-history          # [{sha, date, subject}, ...]
```

This is a read-only convenience wrapper around `git log` against the
snapshot repo. Plain git against the same paths works identically and is
the tool of record for the actual restore below.

## Restoring lost or corrupted task files

1. Find the commit right before the loss:

   ```bash
   sybra-cli tasks-history --limit 50
   ```

2. Restore the tasks dir's contents from that commit. `--git-dir`/`--work-tree`
   point git at the snapshot repo and the live tasks dir respectively — this
   does **not** touch any other git repository on the machine:

   ```bash
   git --git-dir=~/.sybra/tasks-snapshots.git --work-tree=~/.sybra/tasks \
     checkout <sha> -- .
   ```

   Restoring a single file instead of the whole tree:

   ```bash
   git --git-dir=~/.sybra/tasks-snapshots.git --work-tree=~/.sybra/tasks \
     checkout <sha> -- tasks/<task-id>.md
   ```

3. Restart Sybra (or wait for the file watcher to pick up the change) so the
   in-memory task store reflects the restored files.

## Inspecting the full history

Any read-only git command works directly against the snapshot repo:

```bash
git --git-dir=~/.sybra/tasks-snapshots.git --work-tree=~/.sybra/tasks log --oneline
git --git-dir=~/.sybra/tasks-snapshots.git --work-tree=~/.sybra/tasks show <sha>:tasks/<task-id>.md
```

Never run a write/remote operation (`push`, `pull`, `fetch`, `remote add`)
against this repo — the snapshotter has no remote configured and is not
designed to be one; it exists solely as local, at-rest history.

## Troubleshooting

**`sybra-cli tasks-history` reports "snapshotting is disabled or has not run
yet"** — either `task_snapshot.enabled: false` is set, git is not on `PATH`
for the Sybra process, or the app has not completed its first startup pass
yet. Check the Sybra log for `tasksnapshot.*` entries (`tasksnapshot.disabled`,
`tasksnapshot.git_missing`, `tasksnapshot.ensure_repo_failed`,
`tasksnapshot.validate_failed`).

**Stale `index.lock` in the snapshot repo** — like any git repo, an
interrupted git process (e.g. Sybra killed mid-commit) can leave
`~/.sybra/tasks-snapshots.git/index.lock` behind, which blocks every
subsequent commit attempt (logged and swallowed, so this fails silently
until you check the log). Confirm no Sybra process is running, then remove
the stale lock:

```bash
rm ~/.sybra/tasks-snapshots.git/index.lock
```

**The snapshot repo's work-tree does not match `task_snapshot`'s tasks
dir** — this happens if `tasks_dir` was reconfigured on a machine that
already had a `tasks-snapshots.git` from a previous `tasks_dir` value.
Sybra refuses to reuse the mismatched repo and disables snapshotting
(logged as `tasksnapshot.validate_failed`) rather than commit into it. Move
or delete `~/.sybra/tasks-snapshots.git` to let Sybra reinitialize a fresh
repo at the new location — the old repo's history is not migrated
automatically.

## What is out of scope (v1)

- **History pruning.** Full history is kept (`gc.auto=0` bounds growth by
  never triggering an aggressive repack); a time-boxed retention window
  (e.g. 30 days) that rewrites history was deliberately deferred rather than
  shipped destructively. See the task's decision log for the D2 tradeoff.
- **Remote replication.** The snapshot repo is local-only; it is not a
  substitute for off-machine backups.
- **A periodic restore-verification loop.** Nothing currently proves the
  snapshot repo is restorable on a schedule — treat this runbook as the
  manual verification path until that follow-up lands.
