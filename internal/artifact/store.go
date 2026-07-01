package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

// Store is a per-task artifact store backed by ~/.sybra/artifacts/<task-id>/.
// Put uses atomic-rename for blobs; Append uses O_APPEND for streams.
// Correctness never depends on index.json — List does a dir scan.
//
// Lock ordering: the Store's outer mu guards the locks map only. Per-task
// mutexes (returned by lockFor) guard all I/O for that task. Nothing that
// holds a per-task lock may call public methods that re-acquire the same lock.
// Internal helpers that scan/write under an already-held per-task lock use
// scanMetaLocked / rebuildIndexLocked directly.
type Store struct {
	root  string
	mu    sync.Mutex // guards locks map only
	locks map[string]*sync.Mutex
}

// New creates a Store rooted at dir. The directory is created on first write.
func New(dir string) *Store {
	return &Store{root: dir, locks: make(map[string]*sync.Mutex)}
}

// lockFor returns the per-task mutex, creating it if absent.
func (s *Store) lockFor(taskID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	mu, ok := s.locks[taskID]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[taskID] = mu
	}
	return mu
}

// pruneLock removes the per-task lock entry. Call after Delete.
func (s *Store) pruneLock(taskID string) {
	s.mu.Lock()
	delete(s.locks, taskID)
	s.mu.Unlock()
}

// taskDir returns the per-task directory, rejecting hostile IDs and verifying
// path containment as defence in depth even after the regex passes.
func (s *Store) taskDir(taskID string) (string, error) {
	if !validTaskID.MatchString(taskID) {
		return "", fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	dir := filepath.Join(s.root, taskID)
	if !strings.HasPrefix(filepath.Clean(dir)+string(filepath.Separator),
		filepath.Clean(s.root)+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact: task id %q escapes store root", taskID)
	}
	return dir, nil
}

// Put writes a blob artifact atomically. Bytes are written before the meta
// companion so a crash leaves an ignorable orphan, never a meta pointing at
// missing bytes.
func (s *Store) Put(taskID string, a Artifact) (Meta, error) {
	if !validTaskID.MatchString(taskID) {
		return Meta{}, fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	name := a.Name
	if name == "" {
		name = a.Kind.defaultName()
	}
	if !validName.MatchString(name) {
		return Meta{}, fmt.Errorf("artifact: invalid artifact name %q", name)
	}

	dir, err := s.taskDir(taskID)
	if err != nil {
		return Meta{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Meta{}, fmt.Errorf("artifact: mkdir: %w", err)
	}

	m := Meta{
		Name:         name,
		Kind:         a.Kind,
		ProducerRole: a.ProducerRole,
		TaskID:       taskID,
		StepID:       a.StepID,
		CreatedAt:    time.Now().UTC(),
		SourcePath:   a.SourcePath,
		Size:         int64(len(a.Content)),
	}
	metaData, err := json.Marshal(m)
	if err != nil {
		return Meta{}, fmt.Errorf("artifact: marshal meta: %w", err)
	}

	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()

	if err := fsutil.AtomicWrite(filepath.Join(dir, name), a.Content); err != nil {
		return Meta{}, fmt.Errorf("artifact: write blob: %w", err)
	}
	if err := fsutil.AtomicWrite(filepath.Join(dir, name+".meta.json"), metaData); err != nil {
		return Meta{}, fmt.Errorf("artifact: write meta: %w", err)
	}
	s.rebuildIndexLocked(taskID, dir)
	return m, nil
}

// Append appends one JSON-encoded event line to a stream artifact (O_APPEND).
// Creates the stream file and its .meta.json on first call.
func (s *Store) Append(taskID string, kind Kind, event any) error {
	if !validTaskID.MatchString(taskID) {
		return fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	name := kind.defaultName()

	dir, err := s.taskDir(taskID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("artifact: mkdir: %w", err)
	}

	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("artifact: marshal event: %w", err)
	}
	line = append(line, '\n')

	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()

	metaPath := filepath.Join(dir, name+".meta.json")
	if _, statErr := os.Stat(metaPath); errors.Is(statErr, os.ErrNotExist) {
		m := Meta{
			Name:      name,
			Kind:      kind,
			TaskID:    taskID,
			CreatedAt: time.Now().UTC(),
			Stream:    true,
		}
		metaData, mErr := json.Marshal(m)
		if mErr != nil {
			return fmt.Errorf("artifact: marshal meta: %w", mErr)
		}
		if wErr := fsutil.AtomicWrite(metaPath, metaData); wErr != nil {
			return fmt.Errorf("artifact: write meta: %w", wErr)
		}
	}

	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("artifact: open stream: %w", err)
	}
	_, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("artifact: write stream: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("artifact: close stream: %w", closeErr)
	}

	s.rebuildIndexLocked(taskID, dir)
	return nil
}

// List returns metadata for all artifacts for a task, sorted by CreatedAt.
// A missing task directory returns an empty slice, not an error.
func (s *Store) List(taskID string) ([]Meta, error) {
	if !validTaskID.MatchString(taskID) {
		return nil, fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	dir, err := s.taskDir(taskID)
	if err != nil {
		return nil, err
	}
	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	return s.scanMetaLocked(taskID, dir)
}

// scanMetaLocked scans the task directory for *.meta.json files and parses
// them. Caller must hold the per-task lock. Malformed rows are skipped with a
// warning log. Missing dir returns an empty slice, nil error.
func (s *Store) scanMetaLocked(taskID, dir string) ([]Meta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifact: readdir %s: %w", taskID, err)
	}

	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		data, rErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rErr != nil {
			slog.Warn("artifact.meta.read-err", "task_id", taskID, "file", e.Name(), "err", rErr)
			continue
		}
		var m Meta
		if jErr := json.Unmarshal(data, &m); jErr != nil {
			slog.Warn("artifact.meta.parse-err", "task_id", taskID, "file", e.Name(), "err", jErr)
			continue
		}
		metas = append(metas, m)
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.Before(metas[j].CreatedAt)
	})
	return metas, nil
}

// Read returns the raw bytes and metadata for an artifact.
func (s *Store) Read(taskID, name string) ([]byte, Meta, error) {
	if !validTaskID.MatchString(taskID) {
		return nil, Meta{}, fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	if !validName.MatchString(name) {
		return nil, Meta{}, fmt.Errorf("artifact: invalid artifact name %q", name)
	}
	dir, err := s.taskDir(taskID)
	if err != nil {
		return nil, Meta{}, err
	}

	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Meta{}, fmt.Errorf("artifact: %s/%s: %w", taskID, name, ErrNotFound)
		}
		return nil, Meta{}, fmt.Errorf("artifact: read blob: %w", err)
	}
	metaData, err := os.ReadFile(filepath.Join(dir, name+".meta.json"))
	if err != nil {
		return nil, Meta{}, fmt.Errorf("artifact: read meta: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(metaData, &m); err != nil {
		return nil, Meta{}, fmt.Errorf("artifact: parse meta: %w", err)
	}
	return data, m, nil
}

// Delete removes all artifacts for a task and prunes the lock-map entry.
// Ignores a missing directory (idempotent).
func (s *Store) Delete(taskID string) error {
	dir, err := s.taskDir(taskID)
	if err != nil {
		return err
	}
	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("artifact: delete: %w", err)
	}
	s.pruneLock(taskID)
	return nil
}

// Reindex rebuilds index.json from the current *.meta.json files.
// Public repair tool — never call while already holding the per-task lock.
func (s *Store) Reindex(taskID string) error {
	if !validTaskID.MatchString(taskID) {
		return fmt.Errorf("artifact: invalid task id %q", taskID)
	}
	dir, err := s.taskDir(taskID)
	if err != nil {
		return err
	}
	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	s.rebuildIndexLocked(taskID, dir)
	return nil
}

// rebuildIndexLocked writes a derived index.json from current *.meta.json
// files. Caller must hold the per-task lock. Errors are logged, not returned —
// index.json is a convenience cache, not the source of truth.
func (s *Store) rebuildIndexLocked(taskID, dir string) {
	metas, err := s.scanMetaLocked(taskID, dir)
	if err != nil {
		slog.Warn("artifact.index.scan-err", "task_id", taskID, "err", err)
		return
	}
	data, err := json.Marshal(metas)
	if err != nil {
		slog.Warn("artifact.index.marshal-err", "task_id", taskID, "err", err)
		return
	}
	if wErr := fsutil.AtomicWrite(filepath.Join(dir, "index.json"), data); wErr != nil {
		slog.Warn("artifact.index.write-err", "task_id", taskID, "err", wErr)
	}
}

// ListTaskIDs returns the task IDs that have an artifact directory.
func (s *Store) ListTaskIDs() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifact: list task ids: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
