package task

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/fsutil"
)

// CodeReviewStore persists code-review reports as a plain-markdown sidecar
// next to the task file. Mirrors PlanCritiqueStore.
type CodeReviewStore struct {
	dir string
}

func NewCodeReviewStore(dir string) *CodeReviewStore {
	return &CodeReviewStore{dir: dir}
}

func (s *CodeReviewStore) sidecarPath(taskID string) string {
	return filepath.Join(s.dir, taskID+".review.md")
}

// Read returns the review content for a task. Returns ("", nil) if no review exists.
func (s *CodeReviewStore) Read(taskID string) (string, error) {
	data, err := os.ReadFile(s.sidecarPath(taskID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read code review: %w", err)
	}
	return string(data), nil
}

// Write persists review content for a task. An empty content string deletes the sidecar.
func (s *CodeReviewStore) Write(taskID, content string) error {
	if content == "" {
		return s.Delete(taskID)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID), []byte(content)); err != nil {
		return fmt.Errorf("write code review: %w", err)
	}
	return nil
}

// Delete removes the review sidecar for a task. Ignores not-exist errors.
func (s *CodeReviewStore) Delete(taskID string) error {
	if err := os.Remove(s.sidecarPath(taskID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete code review: %w", err)
	}
	return nil
}
