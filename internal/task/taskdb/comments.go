package taskdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/google/uuid"
)

// commentCallTimeout bounds a single CommentStore call, the same reasoning
// taskQueryTimeout documents for every other unbounded caller into this
// package.
const commentCallTimeout = taskQueryTimeout

// CommentStore adapts SQLStore's task_sidecars table to task.CommentPersistence, reusing the exact row a file-backed task's imported .comments.json sidecar already lands in (kind=SidecarComments, name="") rather than adding a second table for the same content.
type CommentStore struct {
	db *db.DB
}

// NewCommentStore returns the database-backed comment store.
func NewCommentStore(database *db.DB) *CommentStore {
	return &CommentStore{db: database}
}

const (
	selectCommentsSidecar = `SELECT content FROM task_sidecars WHERE task_id = ? AND kind = ? AND name = ''`

	// ensureCommentsRow guarantees a row exists before it is locked: a
	// SELECT ... FOR UPDATE against a row that does not exist yet locks
	// nothing on postgres, so two concurrent first-comment writers for the
	// same task would both read "no comments" and one would silently lose
	// the other's write on the upsert that follows.
	ensureCommentsRow = `INSERT INTO task_sidecars (task_id, kind, name, updated_at, content)
		VALUES (?, ?, '', ?, '[]')
		ON CONFLICT (task_id, kind, name) DO NOTHING`
)

func (s *CommentStore) List(taskID string) ([]task.ReviewComment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commentCallTimeout)
	defer cancel()
	var content string
	err := s.db.QueryRowContext(ctx, s.db.Rebind(selectCommentsSidecar), taskID, SidecarComments).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return []task.ReviewComment{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read comments: %w", err)
	}
	return decodeComments(content)
}

func (s *CommentStore) Add(taskID string, line int, body string) (task.ReviewComment, error) {
	created := task.ReviewComment{
		ID:        uuid.NewString()[:8],
		Line:      line,
		Body:      body,
		Resolved:  false,
		CreatedAt: time.Now().UTC(),
	}
	err := s.mutate(taskID, func(comments []task.ReviewComment) ([]task.ReviewComment, error) {
		return append(comments, created), nil
	})
	if err != nil {
		return task.ReviewComment{}, err
	}
	return created, nil
}

func (s *CommentStore) Resolve(taskID, commentID string) error {
	return s.mutate(taskID, func(comments []task.ReviewComment) ([]task.ReviewComment, error) {
		for i := range comments {
			if comments[i].ID == commentID {
				comments[i].Resolved = true
				return comments, nil
			}
		}
		return nil, fmt.Errorf("comment %s not found", commentID)
	})
}

func (s *CommentStore) Delete(taskID, commentID string) error {
	return s.mutate(taskID, func(comments []task.ReviewComment) ([]task.ReviewComment, error) {
		filtered := comments[:0]
		for _, c := range comments {
			if c.ID != commentID {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == len(comments) {
			return nil, fmt.Errorf("comment %s not found", commentID)
		}
		return filtered, nil
	})
}

func (s *CommentStore) ResolveAll(taskID string) error {
	return s.mutate(taskID, func(comments []task.ReviewComment) ([]task.ReviewComment, error) {
		for i := range comments {
			comments[i].Resolved = true
		}
		return comments, nil
	})
}

// mutate runs the List-modify-write cycle every write method shares, inside
// one transaction with the row locked for its duration — the same
// atomicity guarantee lockedTaskRow gives task mutations, so a concurrent
// writer for the same task's comments can never read a state this write is
// about to make stale.
func (s *CommentStore) mutate(taskID string, fn func([]task.ReviewComment) ([]task.ReviewComment, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), commentCallTimeout)
	defer cancel()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(ensureCommentsRow), taskID, SidecarComments, db.TimeValue(time.Now().UTC())); err != nil {
			return fmt.Errorf("ensure comments row: %w", err)
		}
		stmt := selectCommentsSidecar
		if s.db.Dialect() == db.Postgres {
			stmt += ` FOR UPDATE`
		}
		var content string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(stmt), taskID, SidecarComments).Scan(&content); err != nil {
			return fmt.Errorf("read comments: %w", err)
		}
		comments, err := decodeComments(content)
		if err != nil {
			return err
		}
		next, err := fn(comments)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("marshal comments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertSidecar),
			taskID, SidecarComments, "", db.TimeValue(time.Now().UTC()), string(encoded)); err != nil {
			return fmt.Errorf("write comments: %w", err)
		}
		return nil
	})
}

func decodeComments(content string) ([]task.ReviewComment, error) {
	if content == "" {
		return []task.ReviewComment{}, nil
	}
	var comments []task.ReviewComment
	if err := json.Unmarshal([]byte(content), &comments); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}
	return comments, nil
}

var _ task.CommentPersistence = (*CommentStore)(nil)
