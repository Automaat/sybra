package workflow

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// Store manages workflow definition files on disk.
type Store struct {
	dir string
}

// NewStore creates a store backed by the given directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workflows dir: %w", err)
	}
	return &Store{dir: dir}, nil
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
	path := filepath.Join(s.dir, id+".yaml")
	return s.parseFile(path)
}

// Save writes a workflow definition to disk.
func (s *Store) Save(def Definition) error {
	if def.ID == "" {
		return fmt.Errorf("workflow ID is required")
	}
	now := time.Now().UTC()
	if def.CreatedAt.IsZero() {
		def.CreatedAt = now
	}
	def.UpdatedAt = now

	data, err := yaml.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal workflow: %w", err)
	}

	path := filepath.Join(s.dir, def.ID+".yaml")
	return fsutil.AtomicWrite(path, data)
}

// Delete removes a workflow definition file.
func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dir, id+".yaml")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("workflow %s not found", id)
		}
		return fmt.Errorf("delete workflow: %w", err)
	}
	return nil
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
	return def, nil
}
