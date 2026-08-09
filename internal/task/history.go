package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
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
)

// appendHistoryTx records one change inside the caller's transaction.
//
// Same transaction as the change by construction: a caller cannot record the
// change and lose the entry, or the reverse, because there is no path here that
// commits one without the other.
func appendHistoryTx(ctx context.Context, database *db.DB, tx *sql.Tx, entry HistoryEntry) error {
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
	if _, err := tx.ExecContext(ctx, database.Rebind(insertHistory),
		entry.TaskID, entry.Actor, entry.Kind,
		db.TimeValue(entry.ChangedAt), fields, entry.Snapshot); err != nil {
		return fmt.Errorf("record history: %w", err)
	}
	return nil
}

// History returns recorded changes matching q, oldest first.
func (s *SQLStore) History(q HistoryQuery) ([]HistoryEntry, error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), taskQueryTimeout)
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
func (s *SQLStore) TaskAt(id string, at time.Time) (Task, error) {
	if s == nil {
		return Task{}, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskQueryTimeout)
	defer cancel()
	var snapshot string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot FROM task_history WHERE task_id = ? AND changed_at <= ?
			ORDER BY changed_at DESC, id DESC LIMIT 1`,
		id, db.TimeValue(at)).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("no recorded state for task %q at %s", id, at.Format(time.RFC3339))
	}
	if err != nil {
		return Task{}, fmt.Errorf("read history: %w", err)
	}
	return ParseBytes([]byte(snapshot))
}

// TrimHistory removes entries older than retention.
//
// Current state is untouched: this table holds only the record of how the board
// got here, so trimming it costs an investigation depth and never a task.
func (s *SQLStore) TrimHistory(retention time.Duration) error {
	if s == nil || retention <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskQueryTimeout)
	defer cancel()
	cutoff := time.Now().UTC().Add(-retention)
	if _, err := s.db.ExecContext(ctx, deleteHistoryBefore, db.TimeValue(cutoff)); err != nil {
		return fmt.Errorf("trim history: %w", err)
	}
	return nil
}
