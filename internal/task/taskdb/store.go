package taskdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
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
	upsertTask = `INSERT INTO tasks (id, status, project_id, title, created_at, updated_at, deleted_at, doc, board_doc, assigned_node, closed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			status = excluded.status, project_id = excluded.project_id,
			title = excluded.title, updated_at = excluded.updated_at,
			deleted_at = excluded.deleted_at, doc = excluded.doc,
			board_doc = excluded.board_doc, assigned_node = excluded.assigned_node,
			closed_at = excluded.closed_at`

	selectTask = `SELECT doc FROM tasks WHERE id = ? AND deleted_at = 0`

	selectTasks        = `SELECT doc FROM tasks WHERE deleted_at = 0 ORDER BY `
	selectBoardTasks   = `SELECT CASE WHEN board_doc = '' THEN doc ELSE board_doc END FROM tasks WHERE deleted_at = 0 ORDER BY `
	selectTasksForNode = `SELECT doc FROM tasks
		WHERE deleted_at = 0 AND (assigned_node = ? OR assigned_node = '')
		AND (status NOT IN (?, ?) OR closed_at = 0 OR closed_at >= ?)
		ORDER BY `

	// Comments are excluded: they are not a task.Task field, so SidecarsFromTask never reinserts them, and a plain "delete every sidecar row for this task" would wipe every review comment on the very next unrelated field write.
	deleteTaskSidecars = `DELETE FROM task_sidecars WHERE task_id = ? AND kind != ?`

	upsertSidecar = `INSERT INTO task_sidecars (task_id, kind, name, updated_at, content)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (task_id, kind, name) DO UPDATE SET
			updated_at = excluded.updated_at, content = excluded.content`

	// insertSidecarIfAbsent is upsertSidecar's non-clobbering twin: a
	// backfill inserting a row it computed from a point-in-time read (e.g.
	// an on-disk file that may be stale by the time the write lands) must
	// never win a race against a concurrent legitimate writer's ON CONFLICT
	// DO UPDATE — checking absence first and inserting second is not atomic
	// against a writer that commits in between, so this makes "insert only
	// if truly still absent" a single statement instead.
	insertSidecarIfAbsent = `INSERT INTO task_sidecars (task_id, kind, name, updated_at, content)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (task_id, kind, name) DO NOTHING`

	selectSidecars = `SELECT kind, name, content FROM task_sidecars WHERE task_id = ? ORDER BY kind, `

	softDeleteTask = `UPDATE tasks SET deleted_at = ? WHERE id = ?`

	restoreTask = `UPDATE tasks SET deleted_at = 0 WHERE id = ?`

	purgeDeletedTasks = `DELETE FROM tasks WHERE deleted_at > 0 AND deleted_at < ?`

	insertTaskIfAbsent = `INSERT INTO tasks (id, status, project_id, title, created_at, updated_at, deleted_at, doc, board_doc, assigned_node, closed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`

	selectMissingBoardProjections = `SELECT id, doc FROM tasks WHERE board_doc = '' ORDER BY id LIMIT 25`
	backfillBoardProjection       = `UPDATE tasks SET board_doc = ?, assigned_node = ?, closed_at = ? WHERE id = ? AND board_doc = ''`
)

func marshalTaskDocuments(t task.Task) (doc, boardDoc []byte, err error) {
	doc, err = task.MarshalStored(t)
	if err != nil {
		return nil, nil, err
	}
	board := t
	board.AgentRuns = slices.Clone(t.AgentRuns)
	for i := range board.AgentRuns {
		board.AgentRuns[i].Prompt = ""
		board.AgentRuns[i].Result = ""
	}
	boardDoc, err = task.MarshalStored(board)
	return doc, boardDoc, err
}

func taskClosedAtValue(t task.Task) int64 {
	if t.ClosedAt == nil {
		return 0
	}
	return db.TimeValue(*t.ClosedAt)
}

// BackfillBoardProjections upgrades rows written before board_doc and the
// node-filter columns existed. It pages legacy documents so transcript-heavy
// boards do not duplicate their complete corpus in memory during startup.
// Each conditional update is race-safe with a concurrent task write: a fresh
// writer fills board_doc and wins permanently. Callers may safely retry after
// cancellation; every completed row is its own checkpoint.
func (s *SQLStore) BackfillBoardProjections(ctx context.Context) error {
	type pending struct{ id, doc string }
	for {
		rows, err := s.db.QueryContext(ctx, selectMissingBoardProjections)
		if err != nil {
			return err
		}
		batch := make([]pending, 0, 25)
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.doc); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, p := range batch {
			boardDoc, assignedNode, closedAt := p.doc, "", int64(0)
			if parsed, err := task.ParseBytes([]byte(p.doc)); err == nil {
				_, projected, marshalErr := marshalTaskDocuments(parsed)
				if marshalErr != nil {
					return fmt.Errorf("marshal task %s board projection: %w", p.id, marshalErr)
				}
				boardDoc = string(projected)
				assignedNode = parsed.AssignedNode
				closedAt = taskClosedAtValue(parsed)
			}
			if boardDoc == "" {
				// Mark an invalid empty legacy document as visited. ListBoard will
				// continue to skip it just as List does, without looping forever.
				boardDoc = "\n"
			}
			if _, err := s.db.ExecContext(ctx, s.db.Rebind(backfillBoardProjection), boardDoc, assignedNode, closedAt, p.id); err != nil {
				return fmt.Errorf("backfill task %s: %w", p.id, err)
			}
		}
	}
}

// ErrIDCollision reports that CreateBy's candidate ID is already taken. The
// caller mints a fresh one and retries — the same shape Store.createNewTask's
// own collision-retry loop gives the file backend.
var ErrIDCollision = errors.New("task id already exists")

// CreateBy inserts t as a brand-new task, refusing to silently overwrite an
// existing row the way PutBy's upsert would — the guarantee Create needs
// that Update/Put do not. t.ID is the caller's candidate; on ErrIDCollision
// the caller mints a new one and calls again.
func (s *SQLStore) CreateBy(ctx context.Context, t task.Task, sidecars []Sidecar, actor string) (task.Task, error) {
	if s == nil {
		return task.Task{}, errors.New("task store is not configured")
	}
	if t.ID == "" {
		return task.Task{}, errors.New("task store: record has no id")
	}
	doc, boardDoc, err := marshalTaskDocuments(t)
	if err != nil {
		return task.Task{}, fmt.Errorf("marshal task: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(insertTaskIfAbsent),
			t.ID, string(t.Status), t.ProjectID, t.Title,
			db.TimeValue(t.CreatedAt), db.TimeValue(t.UpdatedAt), int64(0), string(doc), string(boardDoc), t.AssignedNode, taskClosedAtValue(t))
		if err != nil {
			return fmt.Errorf("write task: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrIDCollision
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
			TaskID: t.ID, Actor: actor, Kind: ChangeCreated, Snapshot: string(doc),
		})
	})
	if err != nil {
		return task.Task{}, err
	}
	return t, nil
}

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
	doc, boardDoc, err := marshalTaskDocuments(t)
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
			db.TimeValue(t.CreatedAt), db.TimeValue(t.UpdatedAt), int64(0), string(doc), string(boardDoc), t.AssignedNode, taskClosedAtValue(t)); err != nil {
			return fmt.Errorf("write task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteTaskSidecars), t.ID, SidecarComments); err != nil {
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
		return task.Task{}, nil, fmt.Errorf("task %q not found: %w", id, os.ErrNotExist)
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

// ListBoard returns the compact document persisted for board cards. Unlike
// List, it never transfers or parses lifetime prompt/result transcripts.
func (s *SQLStore) ListBoard(ctx context.Context) ([]task.Task, error) {
	return s.listDocuments(ctx, selectBoardTasks+s.db.OrderText("id"))
}

// ListForNode filters in SQL before loading full task documents, so cluster
// reconciliation cost follows a node's active work rather than board history.
func (s *SQLStore) ListForNode(ctx context.Context, node string, closedSince time.Time) ([]task.Task, error) {
	query := selectTasksForNode + s.db.OrderText("id")
	return s.listDocuments(ctx, query, node, string(task.StatusDone), string(task.StatusCancelled), db.TimeValue(closedSince))
}

func (s *SQLStore) listDocuments(ctx context.Context, query string, args ...any) ([]task.Task, error) {
	if s == nil {
		return nil, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list task documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.Task
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t, err := task.ParseBytes([]byte(doc))
		if err == nil {
			out = append(out, t)
		}
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

// PutFnBy atomically reads a task and its sidecars, lets fn compute the
// replacement, and writes both back with a matching history entry — the same
// read-under-lock-then-write atomicity DeleteBy/RestoreBy give a fixed
// transition, generalized to an arbitrary field change. The whole cycle runs
// inside one transaction rather than a separately held lock spanning several
// calls, so there is no second lock primitive to nest under PutBy/DeleteBy's
// own and no risk of the self-deadlock a held-lock-plus-separate-query design
// hits on SQLite's immediate write transaction.
//
// fn receives the sidecar-populated task exactly as Get would return it, so
// callers that mutate plain Task fields never need to know sidecars are
// stored separately; changed is the field-name list recorded in history.
func (s *SQLStore) PutFnBy(ctx context.Context, id, actor string, fn func(cur task.Task) (next task.Task, changed []string, err error)) (task.Task, error) {
	if s == nil {
		return task.Task{}, errors.New("task store is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, taskQueryTimeout)
	defer cancel()
	var result task.Task
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		doc, deletedAt, found, err := s.lockedTaskRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if !found || deletedAt > 0 {
			return fmt.Errorf("task %q not found: %w", id, os.ErrNotExist)
		}
		cur, err := task.ParseBytes([]byte(doc))
		if err != nil {
			return fmt.Errorf("parse task: %w", err)
		}
		sidecars, err := s.sidecarsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		ApplySidecars(&cur, sidecars)

		next, changed, err := fn(cur)
		if err != nil {
			return err
		}
		next.ID = id

		nextDoc, boardDoc, err := marshalTaskDocuments(next)
		if err != nil {
			return fmt.Errorf("marshal task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertTask),
			next.ID, string(next.Status), next.ProjectID, next.Title,
			db.TimeValue(next.CreatedAt), db.TimeValue(next.UpdatedAt), int64(0), string(nextDoc), string(boardDoc), next.AssignedNode, taskClosedAtValue(next)); err != nil {
			return fmt.Errorf("write task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteTaskSidecars), next.ID, SidecarComments); err != nil {
			return fmt.Errorf("clear sidecars: %w", err)
		}
		now := db.TimeValue(time.Now().UTC())
		for _, sc := range SidecarsFromTask(next) {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertSidecar),
				next.ID, sc.Kind, sc.Name, now, sc.Content); err != nil {
				return fmt.Errorf("write sidecar %s: %w", sc.Kind, err)
			}
		}
		if err := appendHistoryTx(ctx, s.db, tx, HistoryEntry{
			TaskID: next.ID, Actor: actor, Kind: ChangeUpdated, Fields: changed,
			Snapshot: string(nextDoc),
		}); err != nil {
			return err
		}
		result = next
		return nil
	})
	return result, err
}

// sidecarsTx is sidecars run inside an already-open transaction, for callers
// (PutFnBy) that need a consistent read alongside the row lock they are
// already holding rather than a second, unlocked connection.
func (s *SQLStore) sidecarsTx(ctx context.Context, tx *sql.Tx, id string) ([]Sidecar, error) {
	rows, err := tx.QueryContext(ctx, s.db.Rebind(selectSidecars+s.db.OrderText("name")), id)
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

// lockedTaskRow reads a task's document and deletion state for a read-modify-write, taking the row's write lock on the way in.
//
// Without FOR UPDATE, postgres' READ COMMITTED lets a concurrent PutBy commit between this read and the matching write, so the history entry would carry a snapshot that was already superseded — a deletion record showing a state the task had left. SQLite takes the same exclusion from its immediate transaction. found is false when no such task exists, which every caller treats as nothing to do.
func (s *SQLStore) lockedTaskRow(ctx context.Context, tx *sql.Tx, id string) (doc string, deletedAt int64, found bool, err error) {
	stmt := `SELECT doc, deleted_at FROM tasks WHERE id = ?`
	if s.db.Dialect() == db.Postgres {
		stmt += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, s.db.Rebind(stmt), id).Scan(&doc, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("read task: %w", err)
	}
	return doc, deletedAt, true, nil
}

// lockedTask is lockedTaskRow's boolean-deleted shape, kept for DeleteBy/RestoreBy.
func (s *SQLStore) lockedTask(ctx context.Context, tx *sql.Tx, id string) (doc string, deleted, found bool, err error) {
	doc, deletedAt, found, err := s.lockedTaskRow(ctx, tx, id)
	return doc, deletedAt > 0, found, err
}
