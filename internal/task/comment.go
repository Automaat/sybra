package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// CommentStore persists review comments as a JSON sidecar next to the task file.
type CommentStore struct {
	dir string
}

func NewCommentStore(dir string) *CommentStore {
	return &CommentStore{dir: dir}
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
	comments, err := s.List(taskID)
	if err != nil {
		return err
	}
	for i := range comments {
		comments[i].Resolved = true
	}
	return s.write(taskID, comments)
}

// DeleteAll removes the sidecar file for a task (called on task deletion).
func (s *CommentStore) DeleteAll(taskID string) error {
	path := s.sidecarPath(taskID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete comments: %w", err)
	}
	return nil
}

func (s *CommentStore) write(taskID string, comments []ReviewComment) error {
	data, err := json.MarshalIndent(comments, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal comments: %w", err)
	}
	if err := os.WriteFile(s.sidecarPath(taskID), data, 0o644); err != nil {
		return fmt.Errorf("write comments: %w", err)
	}
	return nil
}
