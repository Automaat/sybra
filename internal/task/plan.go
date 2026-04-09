package task

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/synapse/internal/fsutil"
)

// PlanStore persists plan content as a plain-markdown sidecar next to the task file.
type PlanStore struct {
	dir string
}

func NewPlanStore(dir string) *PlanStore {
	return &PlanStore{dir: dir}
}

func (s *PlanStore) sidecarPath(taskID string) string {
	return filepath.Join(s.dir, taskID+".plan.md")
}

// Read returns the plan content for a task. Returns ("", nil) if no plan exists.
func (s *PlanStore) Read(taskID string) (string, error) {
	data, err := os.ReadFile(s.sidecarPath(taskID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read plan: %w", err)
	}
	return string(data), nil
}

// Write persists plan content for a task. An empty content string deletes the sidecar.
func (s *PlanStore) Write(taskID, content string) error {
	if content == "" {
		return s.Delete(taskID)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID), []byte(content)); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// Delete removes the plan sidecar for a task. Ignores not-exist errors.
func (s *PlanStore) Delete(taskID string) error {
	if err := os.Remove(s.sidecarPath(taskID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete plan: %w", err)
	}
	return nil
}
