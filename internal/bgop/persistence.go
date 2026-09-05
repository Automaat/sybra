package bgop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/Automaat/sybra/internal/fsutil"
)

// Persistence is where a tracker's operations survive a restart.
//
// Save takes the whole set rather than one record, because that is what the tracker has always written: the set on disk is the set in memory, and an operation dropped for age disappears by not being in the next Save. A store that persisted individual records would need its own eviction path to match.
//
// Missing state is not an error. A first start has nothing to load, and reporting that as a failure would put a warning in the log of every fresh install.
type Persistence interface {
	Load() ([]Operation, error)
	Save(ops []Operation) error
}

// FilePersistence keeps operations in one JSON document.
type FilePersistence struct {
	path string
}

// NewFilePersistence returns the file-backed store, writing to path.
func NewFilePersistence(path string) *FilePersistence {
	return &FilePersistence{path: path}
}

// Path is the document this store writes, for a caller that reports where state lives.
func (p *FilePersistence) Path() string { return p.path }

// Load reads the persisted set, or nothing when the file has never been written.
func (p *FilePersistence) Load() ([]Operation, error) {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read background operations: %w", err)
	}
	var ops []Operation
	if err := json.Unmarshal(data, &ops); err != nil {
		return nil, fmt.Errorf("unmarshal background operations: %w", err)
	}
	return ops, nil
}

// Save replaces the persisted set.
func (p *FilePersistence) Save(ops []Operation) error {
	data, err := json.MarshalIndent(ops, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal background operations: %w", err)
	}
	if err := fsutil.AtomicWrite(p.path, data); err != nil {
		return fmt.Errorf("write background operations: %w", err)
	}
	return nil
}

var _ Persistence = (*FilePersistence)(nil)
