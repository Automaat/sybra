package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
	"github.com/Automaat/sybra/internal/task"
)

// BackfillDomain names the sidecar-suffix backfill in the import marker
// table, distinct from ImportDomain so it runs exactly once independently
// of whether the main task import already ran.
const BackfillDomain = "tasks_sidecar_backfill_v1"

// backfillSuffixes are the three sidecarsOnDisk entries that were wrong
// before this fix (see import.go's comment on the corrected map) — the
// only kinds a board that already ran Import under the old suffixes could
// be missing.
var backfillSuffixes = map[string]string{
	".plan-contract.json": SidecarPlanContract,
	".review.md":          SidecarCodeReview,
	".spec-decision.md":   SidecarSpecDecision,
}

// BackfillMissingSidecarKinds recovers PlanContract/CodeReview/SpecDecision
// sidecars for a board whose task import already ran under the wrong
// on-disk suffixes fixed alongside this function (sidecarsOnDisk previously
// looked for .plan-contract.md, .code-review.md, and .spec-decisions.md —
// filenames the file store never wrote — so those three kinds silently
// never imported for any install that migrated before the fix).
// dbimport.Once's own exactly-once marker means a board that already
// imported never gets a second chance at these three kinds without this.
//
// Only inserts a sidecar row that is genuinely absent for its (task_id,
// kind): a row already present means either the original import correctly
// picked it up (a kind not on the broken list) or live DB-backend usage
// since the original import wrote a real, current value — either way,
// overwriting it with a potentially stale on-disk file would be a
// regression, not a fix. A task that never had one of these three sidecars
// on disk in the first place stays with no row either way, identical to
// how the main import already treats a task with no sidecar file.
//
// The write is a single ON CONFLICT DO NOTHING statement, not a separate existence check followed by a write: dbimport.Once's own lock only excludes other Once callers under the same key, never an ordinary PutBy/PutFnBy, so a two-step check-then-insert could otherwise land after a concurrent legitimate write already committed and clobber it with the stale value this call read earlier. In practice this call runs from App.Startup's openTaskPersistence, before a.tasks (and therefore every PutBy/PutFnBy path) exists at all, so the two never actually overlap today — the atomic insert is a guarantee that holds regardless of that ordering, not a substitute for it.
//
// This only protects against a concurrent PutBy/PutFnBy that is itself writing real content for the kind in question, not against a subsequent PutBy whose caller supplies no content for that kind at all: PutBy's sidecar write reinserts only what SidecarsFromTask computes from the given Task, which skips an empty field, so a caller that clears or never populated a sidecar string still deletes any row already there, backfilled or not — see the leader-follower mirror's Merge, which pushes a follower's sidecar fields, including empty ones, as authoritative. That is pre-existing PutBy/Merge behavior this function does not change; it only guarantees it can never itself be the cause of clobbering live content.
func BackfillMissingSidecarKinds(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, BackfillDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read tasks dir: %w", err)
		}

		// task.IsSidecarFile excludes every sidecar file sharing the directory — a plain "<id>.md" never matches its fixed suffix table (".plan.md", ".plan-draft.*.md", ...) — so this filters down to primary task files without needing a full task.Parse just to derive an id.
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".md" || task.IsSidecarFile(name) {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)

		written := 0
		for _, name := range names {
			taskID := strings.TrimSuffix(name, ".md")
			for suffix, kind := range backfillSuffixes {
				data, err := os.ReadFile(filepath.Join(dir, taskID+suffix))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					logger.Warn("task.import.sidecar_backfill_unreadable", "task_id", taskID, "kind", kind, "err", err,
						"reason", "this sidecar is left un-backfilled; every other task and kind still runs")
					continue
				}
				// insertSidecarIfAbsent, not a check-then-insert: a SELECT
				// followed by a separate INSERT is not atomic against a
				// concurrent legitimate PutBy/PutFnBy committing a real row
				// for this (task_id, kind) in between the two — this
				// backfill's own InTxLocked advisory lock only excludes
				// other Once callers under the same key, never an ordinary
				// task write. A single ON CONFLICT DO NOTHING closes that
				// window: whichever writer's row lands first wins, and the
				// other becomes a no-op, so this can never overwrite a
				// value a concurrent writer just committed.
				res, err := tx.ExecContext(ctx, database.Rebind(insertSidecarIfAbsent),
					taskID, kind, "", db.TimeValue(time.Now().UTC()), string(data))
				if err != nil {
					return 0, fmt.Errorf("insert backfilled sidecar: %w", err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					return 0, fmt.Errorf("count backfilled sidecar rows: %w", err)
				}
				if n == 0 {
					continue
				}
				written++
				logger.Info("task.import.sidecar_backfilled", "task_id", taskID, "kind", kind)
			}
		}
		return written, nil
	})
}
