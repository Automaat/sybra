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
		return nil
	})
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

// Delete marks a task deleted without removing it.
//
// Recoverable until retention passes, which is what the trash directory gave:
// an accidental delete is the one mistake an operator cannot undo by hand once
// the row is gone.
func (s *SQLStore) Delete(ctx context.Context, id string) error {
	if s == nil {
		return errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, softDeleteTask, db.TimeValue(time.Now().UTC()), id); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

// Restore brings a deleted task back while it is still within retention.
func (s *SQLStore) Restore(ctx context.Context, id string) error {
	if s == nil {
		return errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, restoreTask, id); err != nil {
		return fmt.Errorf("restore task: %w", err)
	}
	return nil
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
