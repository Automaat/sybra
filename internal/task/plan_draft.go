package task

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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

	// Negative-cache index over which task IDs own ≥1 draft on disk. Drafts
	// only exist for tasks that went through (dual-)planning, so the vast
	// majority of tasks have none — yet List was called on every Store.Get
	// and scanned the whole tasks dir (~one lstat per file) to discover that.
	// Once a List call has scanned the dir, `have` is authoritative until the
	// next mutation, letting draft-less tasks short-circuit with zero syscalls.
	// `gen` bumps on every mutation so a List that raced a write never adopts
	// a stale index (guards against a false "no drafts" answer).
	mu      sync.Mutex
	have    map[string]struct{}
	indexed bool
	gen     uint64
}

func NewPlanDraftStore(dir string) *PlanDraftStore {
	return &PlanDraftStore{dir: dir}
}

// invalidateIndex drops the cached have-set. Called after every local
// mutation and by Store.InvalidatePath when the watcher sees an external
// change to a plan-draft sidecar.
func (s *PlanDraftStore) invalidateIndex() {
	s.mu.Lock()
	s.gen++
	s.indexed = false
	s.have = nil
	s.mu.Unlock()
}

// parsePlanDraftName splits a sidecar basename into its owning task ID and
// draft name. ok is false for any file that is not a valid plan-draft sidecar.
func parsePlanDraftName(base string) (owner, name string, ok bool) {
	if !strings.HasSuffix(base, ".md") {
		return "", "", false
	}
	idx := strings.Index(base, PlanDraftSidecarPrefix)
	if idx <= 0 {
		return "", "", false
	}
	name = strings.TrimSuffix(base[idx+len(PlanDraftSidecarPrefix):], ".md")
	if name == "" || !validDraftName.MatchString(name) {
		return "", "", false
	}
	return base[:idx], name, true
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
	s.invalidateIndex()
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
	s.invalidateIndex()
	return nil
}

// List returns all drafts for the task as map[name]content. Empty map when
// no drafts exist.
func (s *PlanDraftStore) List(taskID string) (map[string]string, error) {
	// Fast path: a fully-scanned index that doesn't list this task means it
	// has no drafts — return without touching the filesystem.
	s.mu.Lock()
	indexed := s.indexed
	_, has := s.have[taskID]
	startGen := s.gen
	s.mu.Unlock()
	if indexed && !has {
		return map[string]string{}, nil
	}

	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		// No dir → nobody has drafts. Record that so later calls short-circuit.
		s.adoptIndex(startGen, map[string]struct{}{})
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list plan drafts: %w", err)
	}
	out := make(map[string]string)
	// Build the full owner set in the same pass so the index becomes
	// authoritative for every task, not just this one.
	fresh := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		owner, name, ok := parsePlanDraftName(e.Name())
		if !ok {
			continue
		}
		fresh[owner] = struct{}{}
		if owner != taskID {
			continue
		}
		data, rErr := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if rErr != nil {
			return nil, fmt.Errorf("read plan draft %q: %w", name, rErr)
		}
		out[name] = string(data)
	}
	s.adoptIndex(startGen, fresh)
	return out, nil
}

// adoptIndex installs a freshly-scanned have-set, but only if no mutation
// raced the scan (gen unchanged). On a race it discards the scan and leaves
// the index invalid so the next List rebuilds — never caching a stale set.
func (s *PlanDraftStore) adoptIndex(startGen uint64, fresh map[string]struct{}) {
	s.mu.Lock()
	if s.gen == startGen {
		s.have = fresh
		s.indexed = true
	}
	s.mu.Unlock()
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
	removed := false
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
		removed = true
	}
	if removed {
		s.invalidateIndex()
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
