package task

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/fsutil"
)

// PlanCritiqueStore persists plan-critique reports as a plain-markdown sidecar
// next to the task file. Mirrors PlanStore.
type PlanCritiqueStore struct {
	dir string
}

func NewPlanCritiqueStore(dir string) *PlanCritiqueStore {
	return &PlanCritiqueStore{dir: dir}
}

func (s *PlanCritiqueStore) sidecarPath(taskID string) string {
	return filepath.Join(s.dir, taskID+".plan-critique.md")
}

// Read returns the critique content for a task. Returns ("", nil) if no critique exists.
func (s *PlanCritiqueStore) Read(taskID string) (string, error) {
	data, err := os.ReadFile(s.sidecarPath(taskID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read plan critique: %w", err)
	}
	return string(data), nil
}

// Write persists critique content for a task. An empty content string deletes the sidecar.
func (s *PlanCritiqueStore) Write(taskID, content string) error {
	if content == "" {
		return s.Delete(taskID)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID), []byte(content)); err != nil {
		return fmt.Errorf("write plan critique: %w", err)
	}
	return nil
}

// Delete removes the critique sidecar for a task. Ignores not-exist errors.
func (s *PlanCritiqueStore) Delete(taskID string) error {
	if err := os.Remove(s.sidecarPath(taskID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete plan critique: %w", err)
	}
	return nil
}
