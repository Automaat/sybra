package loopagent

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Store persists LoopAgent records as one YAML file per record under dir.
type Store struct {
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create loop agents dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// List returns all loop agents sorted by Name for stable UI ordering.
func (s *Store) List() ([]LoopAgent, error) {
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read loop agents dir: %w", err)
	}
	out := make([]LoopAgent, 0, len(paths))
	for _, p := range paths {
		la, err := s.readFile(p)
		if err != nil {
			continue
		}
		out = append(out, la)
	}
	slices.SortFunc(out, func(a, b LoopAgent) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Get returns the loop agent with the given ID.
func (s *Store) Get(id string) (LoopAgent, error) {
	path := s.filePath(id)
	return s.readFile(path)
}

// FindByName returns the first loop agent whose Name matches. Used by the
// first-boot seed to stay idempotent.
func (s *Store) FindByName(name string) (LoopAgent, bool) {
	all, err := s.List()
	if err != nil {
		return LoopAgent{}, false
	}
	for i := range all {
		if all[i].Name == name {
			return all[i], true
		}
	}
	return LoopAgent{}, false
}

// Create assigns an ID and timestamps, validates, and writes the record.
func (s *Store) Create(la LoopAgent) (LoopAgent, error) {
	if la.Provider == "" {
		la.Provider = "claude"
	}
	if err := la.Validate(); err != nil {
		return LoopAgent{}, err
	}
	now := time.Now().UTC()
	la.ID = uuid.NewString()[:8]
	la.CreatedAt = now
	la.UpdatedAt = now
	if err := s.writeFile(la); err != nil {
		return LoopAgent{}, err
	}
	return la, nil
}

// Update overwrites mutable fields on an existing record. ID and CreatedAt
// are preserved from the on-disk version regardless of caller input.
func (s *Store) Update(la LoopAgent) (LoopAgent, error) {
	existing, err := s.Get(la.ID)
	if err != nil {
		return LoopAgent{}, err
	}
	la.CreatedAt = existing.CreatedAt
	la.UpdatedAt = time.Now().UTC()
	if la.Provider == "" {
		la.Provider = existing.Provider
	}
	if err := la.Validate(); err != nil {
		return LoopAgent{}, err
	}
	if err := s.writeFile(la); err != nil {
		return LoopAgent{}, err
	}
	return la, nil
}

// Delete removes the record file. Missing files are not an error.
func (s *Store) Delete(id string) error {
	path := s.filePath(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete loop agent: %w", err)
	}
	return nil
}

func (s *Store) filePath(id string) string {
	return filepath.Join(s.dir, id+".yaml")
}

func (s *Store) readFile(path string) (LoopAgent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoopAgent{}, fmt.Errorf("read loop agent: %w", err)
	}
	var la LoopAgent
	if err := yaml.Unmarshal(data, &la); err != nil {
		return LoopAgent{}, fmt.Errorf("parse loop agent: %w", err)
	}
	if la.Provider == "" {
		la.Provider = "claude"
	}
	return la, nil
}

func (s *Store) writeFile(la LoopAgent) error {
	data, err := yaml.Marshal(la)
	if err != nil {
		return fmt.Errorf("marshal loop agent: %w", err)
	}
	return fsutil.AtomicWrite(s.filePath(la.ID), data)
}
