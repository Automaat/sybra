package agentqueue

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
	"gopkg.in/yaml.v3"
)

// store persists queue items as one YAML file per TaskID under dir. dir is
// always injected by the caller (see New) — this package never calls
// config.HomeDir() itself, so it stays agnostic of where Sybra's home lives.
// Every method here is best-effort: callers treat store errors as non-fatal
// and keep the in-memory queue authoritative.
type store struct {
	dir string
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent queue dir %s: %w", dir, err)
	}
	return &store{dir: dir}, nil
}

// safeTaskID reports whether id is safe to use as a filename component: it
// must be non-empty, contain no path separators or "..", and equal its own
// filepath.Base (rejecting anything that could escape dir).
func safeTaskID(id string) bool {
	if id == "" {
		return false
	}
	if strings.ContainsAny(id, "/\\") {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	return filepath.Base(id) == id
}

func (s *store) filePath(taskID string) string {
	return filepath.Join(s.dir, taskID+".yaml")
}

func (s *store) put(it Item) error {
	if !safeTaskID(it.TaskID) {
		return fmt.Errorf("agentqueue: unsafe task id %q", it.TaskID)
	}
	data, err := yaml.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal item: %w", err)
	}
	return fsutil.AtomicWrite(s.filePath(it.TaskID), data)
}

func (s *store) del(taskID string) error {
	if !safeTaskID(taskID) {
		return fmt.Errorf("agentqueue: unsafe task id %q", taskID)
	}
	if err := os.Remove(s.filePath(taskID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete item: %w", err)
	}
	return nil
}

// load reads every item file in dir, skipping and logging any that fail to
// read, parse, or sanitize. On a duplicate TaskID (e.g. leftover files from
// a prior filename scheme) the last one loaded wins.
func (s *store) load(log *slog.Logger) []Item {
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		log.Warn("agentqueue.store.load.list-failed", "dir", s.dir, "err", err)
		return nil
	}

	byID := make(map[string]Item, len(paths))
	for _, p := range paths {
		it, ok := s.loadOne(p, log)
		if !ok {
			continue
		}
		byID[it.TaskID] = it
	}

	out := make([]Item, 0, len(byID))
	for _, it := range byID {
		out = append(out, it)
	}
	return out
}

func (s *store) loadOne(path string, log *slog.Logger) (Item, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("agentqueue.store.load.read-failed", "path", path, "err", err)
		return Item{}, false
	}
	var it Item
	if err := yaml.Unmarshal(data, &it); err != nil {
		log.Warn("agentqueue.store.load.parse-failed", "path", path, "err", err)
		return Item{}, false
	}
	if it.TaskID == "" {
		log.Warn("agentqueue.store.load.empty-task-id", "path", path)
		return Item{}, false
	}
	if filepath.Base(path) != it.TaskID+".yaml" {
		log.Warn("agentqueue.store.load.filename-mismatch", "path", path, "task_id", it.TaskID)
		return Item{}, false
	}
	if _, err := task.ValidatePriority(string(it.Priority)); err != nil {
		log.Warn("agentqueue.store.load.invalid-priority", "path", path, "priority", it.Priority)
		return Item{}, false
	}
	if _, err := task.ValidateStatus(string(it.Status)); err != nil {
		log.Warn("agentqueue.store.load.invalid-status", "path", path, "status", it.Status)
		return Item{}, false
	}
	return it, true
}
