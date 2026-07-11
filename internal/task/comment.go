package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/google/uuid"
)

// ReviewComment is an inline comment on a specific line of a plan.
type ReviewComment struct {
	ID        string    `json:"id"`
	Line      int       `json:"line"` // 1-based line number in plan body
	Body      string    `json:"body"`
	Resolved  bool      `json:"resolved"`
	CreatedAt time.Time `json:"createdAt"`
}

// CommentStore persists review comments as a JSON sidecar next to the task
// file. Every mutation is a List (read) → modify → write sequence, so a
// per-task lock guards the whole critical section — otherwise a concurrent
// reader can catch the sidecar mid-truncation (JSON parse error) and two
// racing writers can silently drop one another's update.
type CommentStore struct {
	dir    string
	locker *fsutil.KeyedLocker
}

func NewCommentStore(dir string) *CommentStore {
	return &CommentStore{dir: dir, locker: fsutil.NewKeyedLocker()}
}

func (s *CommentStore) sidecarPath(taskID string) string {
	return filepath.Join(s.dir, taskID+".comments.json")
}

func (s *CommentStore) List(taskID string) ([]ReviewComment, error) {
	data, err := os.ReadFile(s.sidecarPath(taskID))
	if os.IsNotExist(err) {
		return []ReviewComment{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read comments: %w", err)
	}
	var comments []ReviewComment
	if err := json.Unmarshal(data, &comments); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}
	return comments, nil
}

func (s *CommentStore) Add(taskID string, line int, body string) (ReviewComment, error) {
	unlock, err := s.lock(taskID)
	if err != nil {
		return ReviewComment{}, err
	}
	defer unlock()

	comments, err := s.List(taskID)
	if err != nil {
		return ReviewComment{}, err
	}
	c := ReviewComment{
		ID:        uuid.NewString()[:8],
		Line:      line,
		Body:      body,
		Resolved:  false,
		CreatedAt: time.Now().UTC(),
	}
	comments = append(comments, c)
	if err := s.write(taskID, comments); err != nil {
		return ReviewComment{}, err
	}
	return c, nil
}

func (s *CommentStore) Resolve(taskID, commentID string) error {
	unlock, err := s.lock(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	comments, err := s.List(taskID)
	if err != nil {
		return err
	}
	found := false
	for i := range comments {
		if comments[i].ID == commentID {
			comments[i].Resolved = true
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("comment %s not found", commentID)
	}
	return s.write(taskID, comments)
}

func (s *CommentStore) Delete(taskID, commentID string) error {
	unlock, err := s.lock(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	comments, err := s.List(taskID)
	if err != nil {
		return err
	}
	filtered := comments[:0]
	for _, c := range comments {
		if c.ID != commentID {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == len(comments) {
		return fmt.Errorf("comment %s not found", commentID)
	}
	return s.write(taskID, filtered)
}

// ResolveAll marks every unresolved comment for a task as resolved.
func (s *CommentStore) ResolveAll(taskID string) error {
	unlock, err := s.lock(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	comments, err := s.List(taskID)
	if err != nil {
		return err
	}
	for i := range comments {
		comments[i].Resolved = true
	}
	return s.write(taskID, comments)
}

// lock acquires the per-task write lock for the full List-modify-write
// critical section of taskID's comment sidecar.
func (s *CommentStore) lock(taskID string) (func(), error) {
	unlock, err := s.locker.Lock(taskID, s.sidecarPath(taskID))
	if err != nil {
		return nil, fmt.Errorf("lock comments %s: %w", taskID, err)
	}
	return unlock, nil
}

func (s *CommentStore) write(taskID string, comments []ReviewComment) error {
	data, err := json.MarshalIndent(comments, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal comments: %w", err)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID), data); err != nil {
		return fmt.Errorf("write comments: %w", err)
	}
	return nil
}
