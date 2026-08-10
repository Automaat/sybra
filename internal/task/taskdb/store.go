package taskdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
)

// taskQueryTimeout bounds every statement. The board is read from request
// handlers and written from the workflow engine, neither carrying a context.
const taskQueryTimeout = 30 * time.Second

// Sidecar kinds. The name column separates the several drafts a task can carry
// from the single-valued sidecars, which use an empty name.
const (
	SidecarPlan                = "plan"
	SidecarPlanContract        = "plan_contract"
	SidecarPlanCritique        = "plan_critique"
	SidecarPlanResearch        = "plan_research"
	SidecarPlanDecision        = "plan_decision"
	SidecarPlanBrief           = "plan_brief"
	SidecarCodeReview          = "code_review"
	SidecarCurrentTestFailures = "current_test_failures"
	SidecarAcceptanceLedger    = "acceptance_ledger"
	SidecarSpecDecision        = "spec_decision"
	SidecarPlanDraft           = "plan_draft"
	SidecarComments            = "comments"
)

// Sidecar is one stored companion document.
type Sidecar struct {
	Kind    string
	Name    string
	Content string
}

// SQLStore keeps tasks and their sidecars in the configured database backend.
//
// A task and its sidecars are written in one transaction. That is the whole
// point: as files they were several separate writes, so a crash between them
// left a task disagreeing with its own plan or review, and nothing afterwards
// could tell which of the two was current.
type SQLStore struct {
	db *db.DB
}

// NewSQLStore returns the database-backed task store.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("task store needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const (
	upsertTask = `INSERT INTO tasks (id, status, project_id, title, created_at, updated_at, deleted_at, doc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			status = excluded.status, project_id = excluded.project_id,
			title = excluded.title, updated_at = excluded.updated_at,
			deleted_at = excluded.deleted_at, doc = excluded.doc`

	selectTask = `SELECT doc FROM tasks WHERE id = ? AND deleted_at = 0`

	selectTasks = `SELECT doc FROM tasks WHERE deleted_at = 0 ORDER BY `

	deleteTaskSidecars = `DELETE FROM task_sidecars WHERE task_id = ?`

	upsertSidecar = `INSERT INTO task_sidecars (task_id, kind, name, updated_at, content)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (task_id, kind, name) DO UPDATE SET
			updated_at = excluded.updated_at, content = excluded.content`

	selectSidecars = `SELECT kind, name, content FROM task_sidecars WHERE task_id = ? ORDER BY kind, `

	softDeleteTask = `UPDATE tasks SET deleted_at = ? WHERE id = ?`

	restoreTask = `UPDATE tasks SET deleted_at = 0 WHERE id = ?`

	purgeDeletedTasks = `DELETE FROM tasks WHERE deleted_at > 0 AND deleted_at < ?`
)

// Put stores a task and replaces its sidecars, both in one transaction.
func (s *SQLStore) Put(ctx context.Context, t task.Task, sidecars []Sidecar) error {
	return s.PutBy(ctx, t, sidecars, "", nil)
}

// PutBy stores a task, its sidecars, and the history entry for the change.
//
// actor names who made the change and changed lists the fields they touched;
// both are recorded in the same transaction as the write, so a change that
// landed always has a matching history entry and one that did not has none.
func (s *SQLStore) PutBy(ctx context.Context, t task.Task, sidecars []Sidecar, actor string, changed []string) error {
	if s == nil {
		return errors.New("task store is not configured")
	}
	if t.ID == "" {
		return errors.New("task store: record has no id")
	}
	doc, err := task.MarshalStored(t)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var existing bool
		var seen string
		switch err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM tasks WHERE id = ?`), t.ID).Scan(&seen); {
		case err == nil:
			existing = true
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("read task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertTask),
			t.ID, string(t.Status), t.ProjectID, t.Title,
			db.TimeValue(t.CreatedAt), db.TimeValue(t.UpdatedAt), int64(0), string(doc)); err != nil {
			return fmt.Errorf("write task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteTaskSidecars), t.ID); err != nil {
			return fmt.Errorf("clear sidecars: %w", err)
		}
		now := db.TimeValue(time.Now().UTC())
		for i := range sidecars {
			sc := &sidecars[i]
			if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertSidecar),
				t.ID, sc.Kind, sc.Name, now, sc.Content); err != nil {
				return fmt.Errorf("write sidecar %s: %w", sc.Kind, err)
			}
		}
		return appendHistoryTx(ctx, s.db, tx, HistoryEntry{
			TaskID: t.ID, Actor: actor, Kind: kindFor(existing), Fields: changed,
			Snapshot: string(doc),
		})
	})
}

// kindFor names the change from whether the task was already there.
func kindFor(existed bool) string {
	if existed {
		return ChangeUpdated
	}
	return ChangeCreated
}

// Get returns one task and its sidecars.
func (s *SQLStore) Get(ctx context.Context, id string) (task.Task, []Sidecar, error) {
	if s == nil {
		return task.Task{}, nil, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	var doc string
	err := s.db.QueryRowContext(ctx, selectTask, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, nil, fmt.Errorf("task %q not found", id)
	}
	if err != nil {
		return task.Task{}, nil, fmt.Errorf("read task: %w", err)
	}
	t, err := task.ParseBytes([]byte(doc))
	if err != nil {
		return task.Task{}, nil, fmt.Errorf("parse task: %w", err)
	}
	sidecars, err := s.sidecars(ctx, id)
	if err != nil {
		return task.Task{}, nil, err
	}
	return t, sidecars, nil
}

// List returns every live task, ordered by id so the board is stable.
func (s *SQLStore) List(ctx context.Context) ([]task.Task, error) {
	if s == nil {
		return nil, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectTasks+s.db.OrderText("id"))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.Task
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t, err := task.ParseBytes([]byte(doc))
		if err != nil {
			// Skipped as the directory listing skips a file it cannot parse,
			// rather than costing the caller the whole board.
			continue
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return out, nil
}

func (s *SQLStore) sidecars(ctx context.Context, id string) ([]Sidecar, error) {
	rows, err := s.db.QueryContext(ctx, selectSidecars+s.db.OrderText("name"), id)
	if err != nil {
		return nil, fmt.Errorf("read sidecars: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Sidecar
	for rows.Next() {
		var sc Sidecar
		if err := rows.Scan(&sc.Kind, &sc.Name, &sc.Content); err != nil {
			return nil, fmt.Errorf("scan sidecar: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sidecars: %w", err)
	}
	return out, nil
}

// Delete marks a task deleted without removing it, recording no actor.
// Mirrors the Put/PutBy split: a caller not attributing the change to anyone
// uses this, a real mutation should call DeleteBy.
func (s *SQLStore) Delete(ctx context.Context, id string) error {
	return s.DeleteBy(ctx, id, "")
}

// DeleteBy marks a task deleted without removing it, and names actor in the
// history entry the same way PutBy does for an ordinary write.
//
// Recoverable until retention passes, which is what the trash directory gave:
// an accidental delete is the one mistake an operator cannot undo by hand once
// the row is gone.
func (s *SQLStore) DeleteBy(ctx context.Context, id, actor string) error {
	if s == nil {
		return errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		doc, deleted, found, err := s.lockedTask(ctx, tx, id)
		if err != nil || !found {
			return err
		}
		// Already deleted: a second delete would append a change that did not happen and push the retention window out, keeping the row past the age it should have been trimmed at.
		if deleted {
			return nil
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(softDeleteTask), db.TimeValue(time.Now().UTC()), id); err != nil {
			return fmt.Errorf("delete task: %w", err)
		}
		return appendHistoryTx(ctx, s.db, tx, HistoryEntry{TaskID: id, Actor: actor, Kind: ChangeDeleted, Snapshot: doc})
	})
}

// Restore brings a deleted task back while it is still within retention,
// recording no actor. Mirrors the Put/PutBy split: a caller not attributing
// the change to anyone uses this, a real mutation should call RestoreBy.
func (s *SQLStore) Restore(ctx context.Context, id string) error {
	return s.RestoreBy(ctx, id, "")
}

// RestoreBy brings a deleted task back while it is still within retention,
// and names actor in the history entry the same way PutBy does for an
// ordinary write.
func (s *SQLStore) RestoreBy(ctx context.Context, id, actor string) error {
	if s == nil {
		return errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		doc, deleted, found, err := s.lockedTask(ctx, tx, id)
		if err != nil || !found {
			return err
		}
		// Restoring a task that was never deleted records a transition the board did not make.
		if !deleted {
			return nil
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(restoreTask), id); err != nil {
			return fmt.Errorf("restore task: %w", err)
		}
		return appendHistoryTx(ctx, s.db, tx, HistoryEntry{TaskID: id, Actor: actor, Kind: ChangeRestored, Snapshot: doc})
	})
}

// PurgeDeleted removes tasks deleted longer ago than retention.
func (s *SQLStore) PurgeDeleted(ctx context.Context, retention time.Duration) error {
	if s == nil || retention <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	cutoff := time.Now().UTC().Add(-retention)
	if _, err := s.db.ExecContext(ctx, purgeDeletedTasks, db.TimeValue(cutoff)); err != nil {
		return fmt.Errorf("purge deleted tasks: %w", err)
	}
	return nil
}

// lockedTask reads a task's document and deletion state for a read-modify-write, taking the row's write lock on the way in.
//
// Without FOR UPDATE, postgres' READ COMMITTED lets a concurrent PutBy commit between this read and the matching write, so the history entry would carry a snapshot that was already superseded — a deletion record showing a state the task had left. SQLite takes the same exclusion from its immediate transaction. found is false when no such task exists, which every caller treats as nothing to do.
func (s *SQLStore) lockedTask(ctx context.Context, tx *sql.Tx, id string) (doc string, deleted, found bool, err error) {
	stmt := `SELECT doc, deleted_at FROM tasks WHERE id = ?`
	if s.db.Dialect() == db.Postgres {
		stmt += ` FOR UPDATE`
	}
	// Live rows carry 0 rather than NULL, so deletion is a positive timestamp and not merely a non-null column.
	var deletedAt int64
	if err := tx.QueryRowContext(ctx, s.db.Rebind(stmt), id).Scan(&doc, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf("read task: %w", err)
	}
	return doc, deletedAt > 0, true, nil
}
