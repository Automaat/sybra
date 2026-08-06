package task

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/fsutil"
)

// PlanningSidecarStore persists a single-file sidecar next to the task file:
// the plan, its contract, research, decisions, brief, critique, and the code
// review. suffix picks the file, label names it in errors.
type PlanningSidecarStore struct {
	dir    string
	suffix string
	label  string
}

func NewPlanningSidecarStore(dir, suffix, label string) *PlanningSidecarStore {
	return &PlanningSidecarStore{dir: dir, suffix: suffix, label: label}
}

// sidecarPath validates the id before joining. Nine sidecar stores used to
// join an unchecked id, and ids arrive from outside this process — a cluster
// peer pushes one over HTTP — so the check belongs here, at the one place
// every sidecar path is now built, rather than in each caller.
func (s *PlanningSidecarStore) sidecarPath(taskID string) (string, error) {
	if err := ValidateID(taskID); err != nil {
		return "", fmt.Errorf("%s: %w", s.label, err)
	}
	return filepath.Join(s.dir, taskID+s.suffix), nil
}

// Read returns the sidecar content for a task. Returns ("", nil) when missing.
func (s *PlanningSidecarStore) Read(taskID string) (string, error) {
	path, err := s.sidecarPath(taskID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
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
	path, err := s.sidecarPath(taskID)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(path, []byte(content)); err != nil {
		return fmt.Errorf("write %s: %w", s.label, err)
	}
	return nil
}

// Delete removes the sidecar. Missing files are ignored.
func (s *PlanningSidecarStore) Delete(taskID string) error {
	path, err := s.sidecarPath(taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", s.label, err)
	}
	return nil
}
