package taskdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
)

// Change kinds recorded in the history.
const (
	ChangeCreated  = "created"
	ChangeUpdated  = "updated"
	ChangeDeleted  = "deleted"
	ChangeRestored = "restored"
)

// HistoryEntry is one recorded change to a task.
//
// Snapshot is the whole stored task as it stood after the change. A diff would
// be smaller, but reconstructing a past state from diffs means replaying every
// entry since the beginning and trusting all of them; a snapshot answers "what
// did this look like then" by reading one row, which is the question every
// stuck-task investigation actually starts from.
type HistoryEntry struct {
	ID        int64
	TaskID    string
	Actor     string
	Kind      string
	ChangedAt time.Time
	Fields    []string
	Snapshot  string
}

// HistoryQuery filters a history read. A zero value returns everything, which
// only a small board or a test wants.
type HistoryQuery struct {
	TaskID string
	Actor  string
	Since  time.Time
	Until  time.Time
	Limit  int
}

const (
	insertHistory = `INSERT INTO task_history (task_id, actor, kind, changed_at, fields, snapshot)
		VALUES (?, ?, ?, ?, ?, ?)`

	deleteHistoryBefore = `DELETE FROM task_history WHERE changed_at < ?`

	// COALESCE keeps the newest entry when it alone exceeds the budget:
	// without it the inner query matches no row, min(id) is NULL, and
	// "id < NULL" deletes nothing — so the one task the budget exists to
	// bound would be the one task it never trims.
	deleteHistoryOverBytes = `DELETE FROM task_history WHERE task_id = ? AND id < (
		SELECT COALESCE(
			(SELECT min(id) FROM (
				SELECT id, SUM(length(snapshot)) OVER (
					PARTITION BY task_id ORDER BY id DESC
					ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
				) AS running
				FROM task_history WHERE task_id = ?
			) AS sized WHERE running <= ?),
			(SELECT max(id) FROM task_history WHERE task_id = ?)))`

	deleteHistoryOverCap = `DELETE FROM task_history WHERE task_id = ? AND id < (
		SELECT min(id) FROM (
			SELECT id FROM task_history WHERE task_id = ? ORDER BY id DESC LIMIT ?
		) AS keep)`

	deleteHistoryOverCapAllTasks = `DELETE FROM task_history WHERE id IN (
		SELECT id FROM (
			SELECT id, row_number() OVER (PARTITION BY task_id ORDER BY id DESC) AS rn
			FROM task_history
		) AS ranked WHERE rn > ? LIMIT ?)`
)

// historySweepBatch bounds one statement of the startup sweep.
//
// A board that has been running unbounded holds several GB of entries, and
// deleting them in one statement writes that whole span into the write-ahead
// log before anything can checkpoint it — the sweep would briefly cost more
// than the growth it exists to undo. Batches commit on their own, so the log
// stays bounded and the board keeps serving between them.
const historySweepBatch = 2000

func (s *SQLStore) setHistorySweepBatch(n int) {
	if s == nil {
		return
	}
	s.historySweepBatch = n
}

// DefaultMaxHistoryPerTask bounds how many entries one task keeps.
//
// Every entry carries the whole task document, and the workflow engine rewrites
// a running task on each step claim and completion, so a long-lived task
// accumulates thousands of near-identical multi-hundred-KB snapshots. Left
// unbounded that is the largest table on the board by an order of magnitude,
// and the write volume grows the write-ahead log faster than it can be
// checkpointed until an ordinary board read exceeds taskQueryTimeout.
//
// The cap is per task rather than global because the cost is concentrated: a
// handful of hot tasks hold most of the rows while the median task has a few
// dozen, so trimming the deepest tails leaves every task the recent depth an
// investigation actually reads.
const DefaultMaxHistoryPerTask = 200

// DefaultMaxHistoryBytesPerTask bounds one task's history by size as well as
// by count.
//
// The row cap alone does not bound the table: an entry holds a whole task
// document, and a document carrying plans, reviews and a long acceptance
// ledger runs to tens of kilobytes, so the cap's worth of history is several
// megabytes for ONE task. A board of a thousand such tasks reached 4.9 GB
// holding 1.4 GB of live data, and its reads slowed until the sweeps that
// release umbrella children and re-dispatch stalled tasks timed out.
//
// The newest entry is always kept, however large, so a task whose document
// exceeds the budget on its own still records what changed.
const DefaultMaxHistoryBytesPerTask = 2 << 20

// appendHistoryTx records one change inside the caller's transaction.
//
// Same transaction as the change by construction: a caller cannot record the
// change and lose the entry, or the reverse, because there is no path here that
// commits one without the other.
func (s *SQLStore) appendHistoryTx(ctx context.Context, tx *sql.Tx, entry HistoryEntry) error {
	fields := ""
	if len(entry.Fields) > 0 {
		encoded, err := json.Marshal(entry.Fields)
		if err != nil {
			return fmt.Errorf("encode changed fields: %w", err)
		}
		fields = string(encoded)
	}
	if entry.ChangedAt.IsZero() {
		entry.ChangedAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(insertHistory),
		entry.TaskID, entry.Actor, entry.Kind,
		db.TimeValue(entry.ChangedAt), fields, entry.Snapshot); err != nil {
		return fmt.Errorf("record history: %w", err)
	}
	return s.trimTaskHistoryTx(ctx, tx, entry.TaskID)
}

// trimTaskHistoryTx applies both bounds. They are independent: disabling the
// row cap must not also disable the size budget, since the budget is the one
// that actually bounds the table.
func (s *SQLStore) trimTaskHistoryTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	if limit := s.maxHistoryPerTask; limit >= 0 {
		if limit == 0 {
			limit = DefaultMaxHistoryPerTask
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteHistoryOverCap), taskID, taskID, limit); err != nil {
			return fmt.Errorf("trim task history: %w", err)
		}
	}
	budget := s.maxHistoryBytesPerTask
	if budget < 0 {
		return nil
	}
	if budget == 0 {
		budget = DefaultMaxHistoryBytesPerTask
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteHistoryOverBytes), taskID, taskID, budget, taskID); err != nil {
		return fmt.Errorf("trim task history by size: %w", err)
	}
	return nil
}

// History returns recorded changes matching q, oldest first.
func (s *SQLStore) History(ctx context.Context, q HistoryQuery) ([]HistoryEntry, error) {
	if s == nil {
		return nil, errors.New("task store is not configured")
	}
	stmt := `SELECT id, task_id, actor, kind, changed_at, fields, snapshot FROM task_history WHERE 1 = 1`
	var args []any
	if strings.TrimSpace(q.TaskID) != "" {
		stmt += ` AND task_id = ?`
		args = append(args, q.TaskID)
	}
	if strings.TrimSpace(q.Actor) != "" {
		stmt += ` AND actor = ?`
		args = append(args, q.Actor)
	}
	if !q.Since.IsZero() {
		stmt += ` AND changed_at >= ?`
		args = append(args, db.TimeValue(q.Since))
	}
	if !q.Until.IsZero() {
		stmt += ` AND changed_at <= ?`
		args = append(args, db.TimeValue(q.Until))
	}
	stmt += ` ORDER BY changed_at, id`
	if q.Limit > 0 {
		stmt += ` LIMIT ?`
		args = append(args, q.Limit)
	}

	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HistoryEntry
	for rows.Next() {
		var (
			e         HistoryEntry
			changedAt int64
			fields    string
		)
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Actor, &e.Kind, &changedAt, &fields, &e.Snapshot); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		e.ChangedAt = db.TimeFrom(changedAt)
		if fields != "" {
			if err := json.Unmarshal([]byte(fields), &e.Fields); err != nil {
				// A malformed field list costs the reader that detail, not the
				// entry: the snapshot still answers what the task looked like.
				e.Fields = nil
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return out, nil
}

// TaskAt reconstructs a task as it stood at a moment.
//
// The latest snapshot at or before the moment, which is the state any later
// change started from. Nothing is replayed.
func (s *SQLStore) TaskAt(ctx context.Context, id string, at time.Time) (task.Task, error) {
	if s == nil {
		return task.Task{}, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	var snapshot, kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot, kind FROM task_history WHERE task_id = ? AND changed_at <= ?
			ORDER BY changed_at DESC, id DESC LIMIT 1`,
		id, db.TimeValue(at)).Scan(&snapshot, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, fmt.Errorf("no recorded state for task %q at %s", id, at.Format(time.RFC3339))
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("read history: %w", err)
	}
	// A deletion's snapshot is the document as it stood before the delete, so returning it here would present a task that was not on the board at that moment as if it had been. The caller asked what the board held, and the answer is nothing.
	if kind == ChangeDeleted {
		return task.Task{}, fmt.Errorf("task %q was deleted as of %s", id, at.Format(time.RFC3339))
	}
	return task.ParseBytes([]byte(snapshot))
}

// TrimHistoryOverCap enforces the per-task cap across every task at once.
//
// The per-write trim only reaches a task the next time it is written, so a task
// that stopped changing keeps whatever tail it had accumulated. This is what
// brings an existing board down to the cap, and it is a no-op once it has.
func (s *SQLStore) TrimHistoryOverCap(ctx context.Context) error {
	if s == nil {
		return nil
	}
	limit := s.maxHistoryPerTask
	if limit < 0 {
		return nil
	}
	if limit == 0 {
		limit = DefaultMaxHistoryPerTask
	}
	ctx, cancel := context.WithTimeout(ctx, historySweepTimeout)
	defer cancel()
	batch := s.historySweepBatch
	if batch <= 0 {
		batch = historySweepBatch
	}
	for {
		res, err := s.db.ExecContext(ctx, deleteHistoryOverCapAllTasks, limit, batch)
		if err != nil {
			return fmt.Errorf("trim history over cap: %w", err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("count trimmed history: %w", err)
		}
		if removed == 0 {
			return nil
		}
	}
}

// historySweepTimeout bounds the startup sweeps. They rewrite a table that has
// grown to several GB on a busy board, which is far more than taskQueryTimeout
// allows an ordinary read.
const historySweepTimeout = 10 * time.Minute

// TrimHistory removes entries older than retention.
//
// Current state is untouched: this table holds only the record of how the board
// got here, so trimming it costs an investigation depth and never a task.
func (s *SQLStore) TrimHistory(ctx context.Context, retention time.Duration) error {
	if s == nil || retention <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	cutoff := time.Now().UTC().Add(-retention)
	if _, err := s.db.ExecContext(ctx, deleteHistoryBefore, db.TimeValue(cutoff)); err != nil {
		return fmt.Errorf("trim history: %w", err)
	}
	return nil
}
