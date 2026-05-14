package task

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Automaat/sybra/internal/fsutil"
)

// PlanDraftSidecarPrefix is the on-disk filename prefix for plan-draft sidecars.
// Pattern: <task_id>.plan-draft-<name>.md (e.g. abc123.plan-draft-plan_claude.md).
const PlanDraftSidecarPrefix = ".plan-draft-"

// validDraftName limits names to characters that are safe in filenames AND
// in step IDs (no dots, slashes, spaces). Matches the workflow step-ID
// convention so the engine can pass step IDs through verbatim.
var validDraftName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// PlanDraftStore persists per-provider plan drafts during dual- (or N-)
// provider planning. Each draft is keyed by name (typically the parallel
// child step ID, e.g. "plan_claude") so adding a new provider only requires
// adding a new child to the workflow — no schema changes here.
//
// The convergence step reads all drafts and writes the merged result to the
// canonical Plan sidecar; drafts remain on disk for audit and re-runs.
type PlanDraftStore struct {
	dir string
}

func NewPlanDraftStore(dir string) *PlanDraftStore {
	return &PlanDraftStore{dir: dir}
}

func (s *PlanDraftStore) sidecarPath(taskID, name string) string {
	return filepath.Join(s.dir, taskID+PlanDraftSidecarPrefix+name+".md")
}

// validate guards against draft names that would either escape the task
// directory or produce ambiguous filenames (collisions with the suffix
// scheme used by IsSidecarFile).
func (s *PlanDraftStore) validate(name string) error {
	if name == "" {
		return errors.New("plan draft name is empty")
	}
	if !validDraftName.MatchString(name) {
		return fmt.Errorf("plan draft name %q must match [a-zA-Z0-9_-]+", name)
	}
	return nil
}

// Read returns the draft for (taskID, name). Returns ("", nil) if no draft exists.
func (s *PlanDraftStore) Read(taskID, name string) (string, error) {
	if err := s.validate(name); err != nil {
		return "", err
	}
	data, err := os.ReadFile(s.sidecarPath(taskID, name))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read plan draft %q: %w", name, err)
	}
	return string(data), nil
}

// Write stores the draft. Empty content deletes the sidecar.
func (s *PlanDraftStore) Write(taskID, name, content string) error {
	if err := s.validate(name); err != nil {
		return err
	}
	if content == "" {
		return s.Delete(taskID, name)
	}
	if err := fsutil.AtomicWrite(s.sidecarPath(taskID, name), []byte(content)); err != nil {
		return fmt.Errorf("write plan draft %q: %w", name, err)
	}
	return nil
}

// Delete removes a single draft. Ignores not-exist.
func (s *PlanDraftStore) Delete(taskID, name string) error {
	if err := s.validate(name); err != nil {
		return err
	}
	if err := os.Remove(s.sidecarPath(taskID, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete plan draft %q: %w", name, err)
	}
	return nil
}

// List returns all drafts for the task as map[name]content. Empty map when
// no drafts exist.
func (s *PlanDraftStore) List(taskID string) (map[string]string, error) {
	prefix := taskID + PlanDraftSidecarPrefix
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list plan drafts: %w", err)
	}
	out := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".md") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".md")
		if name == "" || !validDraftName.MatchString(name) {
			continue
		}
		data, rErr := os.ReadFile(filepath.Join(s.dir, base))
		if rErr != nil {
			return nil, fmt.Errorf("read plan draft %q: %w", name, rErr)
		}
		out[name] = string(data)
	}
	return out, nil
}

// DeleteAll removes every draft for the task. Used by callers that need
// to reset the dual-planning state (e.g. before a re-plan iteration).
func (s *PlanDraftStore) DeleteAll(taskID string) error {
	prefix := taskID + PlanDraftSidecarPrefix
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list plan drafts: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".md") {
			continue
		}
		if rmErr := os.Remove(filepath.Join(s.dir, base)); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("delete plan draft %s: %w", base, rmErr)
		}
	}
	return nil
}

// IsPlanDraftFile reports whether a basename matches the plan-draft sidecar
// naming pattern. Used by the sidecar filter in store.go.
func IsPlanDraftFile(base string) bool {
	if !strings.HasSuffix(base, ".md") {
		return false
	}
	idx := strings.Index(base, PlanDraftSidecarPrefix)
	if idx <= 0 {
		return false
	}
	name := strings.TrimSuffix(base[idx+len(PlanDraftSidecarPrefix):], ".md")
	return name != "" && validDraftName.MatchString(name)
}
