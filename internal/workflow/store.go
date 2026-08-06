package workflow

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// Store manages workflow definition files on disk.
type Store struct {
	dir         string
	snapshotDir string
}

var snapshotHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// NewStore creates a store backed by the given directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workflows dir: %w", err)
	}
	snapshotDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		return nil, fmt.Errorf("create workflow snapshots dir: %w", err)
	}
	return &Store{dir: dir, snapshotDir: snapshotDir}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// List returns all workflow definitions.
func (s *Store) List() ([]Definition, error) {
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read workflows dir: %w", err)
	}

	var defs []Definition
	for _, p := range paths {
		d, pErr := s.parseFile(p)
		if pErr != nil {
			slog.Default().Warn("workflow.parse.skip", "file", filepath.Base(p), "err", pErr)
			continue
		}
		defs = append(defs, d)
	}
	return defs, nil
}

// Get returns a workflow definition by ID.
func (s *Store) Get(id string) (Definition, error) {
	path, err := s.safePath(id)
	if err != nil {
		return Definition{}, err
	}
	return s.parseFile(path)
}

// Save writes a workflow definition to disk.
func (s *Store) Save(def Definition) error {
	if def.ID == "" {
		return fmt.Errorf("workflow ID is required")
	}
	path, err := s.safePath(def.ID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if def.CreatedAt.IsZero() {
		def.CreatedAt = now
	}
	def.UpdatedAt = now

	if vErr := def.Validate(); vErr != nil {
		return fmt.Errorf("validate workflow: %w", vErr)
	}

	data, mErr := yaml.Marshal(def)
	if mErr != nil {
		return fmt.Errorf("marshal workflow: %w", mErr)
	}
	return fsutil.AtomicWrite(path, data)
}

// SaveSnapshot persists an immutable workflow definition snapshot keyed by the
// definition's semantic hash. Existing snapshots are preserved as-is.
func (s *Store) SaveSnapshot(def Definition) (string, error) {
	if def.ID == "" {
		return "", fmt.Errorf("workflow ID is required")
	}
	hash, err := def.SemanticHash()
	if err != nil {
		return "", err
	}
	path, err := s.snapshotPath(def.ID, hash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat workflow snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create workflow snapshot dir: %w", err)
	}
	data, err := yaml.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("marshal workflow snapshot: %w", err)
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return "", fmt.Errorf("write workflow snapshot: %w", err)
	}
	return hash, nil
}

// GetSnapshot loads a previously persisted workflow definition snapshot.
func (s *Store) GetSnapshot(workflowID, hash string) (Definition, error) {
	path, err := s.snapshotPath(workflowID, hash)
	if err != nil {
		return Definition{}, err
	}
	return s.parseFile(path)
}

// Delete removes a workflow definition file.
func (s *Store) Delete(id string) error {
	path, err := s.safePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("workflow %s not found", id)
		}
		return fmt.Errorf("delete workflow: %w", err)
	}
	return nil
}

// safePath validates that the resolved path stays under the store directory.
func (s *Store) safePath(id string) (string, error) {
	path := filepath.Clean(filepath.Join(s.dir, id+".yaml"))
	if !strings.HasPrefix(path, filepath.Clean(s.dir)+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid workflow ID %q", id)
	}
	return path, nil
}

func (s *Store) snapshotPath(workflowID, hash string) (string, error) {
	if !snapshotHashPattern.MatchString(hash) {
		return "", fmt.Errorf("invalid workflow snapshot hash %q", hash)
	}
	path := filepath.Clean(filepath.Join(s.snapshotDir, workflowID, hash+".yaml"))
	if !strings.HasPrefix(path, filepath.Clean(s.snapshotDir)+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid workflow ID %q", workflowID)
	}
	return path, nil
}

func (s *Store) parseFile(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			id := strings.TrimSuffix(filepath.Base(path), ".yaml")
			return Definition{}, fmt.Errorf("workflow %s not found", id)
		}
		return Definition{}, fmt.Errorf("read workflow: %w", err)
	}

	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return Definition{}, fmt.Errorf("unmarshal workflow %s: %w", filepath.Base(path), err)
	}
	if vErr := def.Validate(); vErr != nil {
		return Definition{}, fmt.Errorf("validate workflow %s: %w", filepath.Base(path), vErr)
	}
	return def, nil
}
