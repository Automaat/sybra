package task

import "github.com/Automaat/sybra/internal/fsutil"

// migrateLegacyTask stamps ClosedAt for legacy terminal tasks that predate
// the ClosedAt field, and backfills StatusChangedAt for any legacy task
// (terminal or not) that predates that field, rewriting path in place when
// either backfill applies. Detectors like the lost-agent grace window key
// off StatusChangedAt and must never see a permanent zero value on a
// read-only path — List calls this for every task it parses (rather than
// waiting on the next Update/AddRun) so it self-heals regardless of whether
// Migrate has ever run for this store.
func migrateLegacyTask(t *Task, path string) {
	needsMigration := t.StatusChangedAt.IsZero() || (IsTerminalStatus(t.Status) && t.ClosedAt == nil)
	if !needsMigration {
		return
	}
	ts := t.UpdatedAt
	if IsTerminalStatus(t.Status) && t.ClosedAt == nil {
		t.ClosedAt = &ts
	}
	backfillStatusChangedAt(t, ts)
	if data, err := marshalTask(*t, false); err == nil {
		_ = fsutil.AtomicWrite(path, data)
	}
}

// Migrate eagerly runs the legacy per-task backfill (see migrateLegacyTask)
// across every task in the store, so a fresh startup pays that cost once up
// front instead of the first caller of List() paying it lazily. List's own
// self-heal (every task it parses goes through migrateLegacyTask) is the
// safety net this delegates to — Migrate does not replace it, since a task
// written or restored from trash after startup still needs the same
// backfill on its own first read.
func (s *Store) Migrate() error {
	_, err := s.List()
	return err
}
