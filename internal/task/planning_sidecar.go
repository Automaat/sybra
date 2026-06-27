package task

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/fsutil"
)

// PlanningSidecarStore persists planning-support sidecars next to the task file.
// These sidecars hold JSON contracts, research, decision prompts, and briefs.
type PlanningSidecarStore struct {
	dir    string
	suffix string
	label  string
}

func NewPlanningSidecarStore(dir, suffix, label string) *PlanningSidecarStore {
	return &PlanningSidecarStore{dir: dir, suffix: suffix, label: label}
}

func (s *PlanningSidecarStore) sidecarPath(taskID string) string {
	return filepath.Join(s.dir, taskID+s.suffix)
}

// Read returns the sidecar content for a task. Returns ("", nil) when missing.
func (s *PlanningSidecarStore) Read(taskID string) (string, error) {
	data, err := os.ReadFile(s.sidecarPath(taskID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s.label, err)
	}
	return string(data), nil
}

// Write persists sidecar content. Empty content deletes the sidecar.
func (s *PlanningSidecarStore) Write(taskID, content string) error {
	if content == "" {
		return s.Delete(taskID)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID), []byte(content)); err != nil {
		return fmt.Errorf("write %s: %w", s.label, err)
	}
	return nil
}

// Delete removes the sidecar. Missing files are ignored.
func (s *PlanningSidecarStore) Delete(taskID string) error {
	if err := os.Remove(s.sidecarPath(taskID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", s.label, err)
	}
	return nil
}
