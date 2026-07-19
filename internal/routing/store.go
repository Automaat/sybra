package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Automaat/sybra/internal/fsutil"
	"gopkg.in/yaml.v3"
)

const overlayFileName = "overlay.yaml"

// Store is the atomic, single-file, local persistence for the routing
// overlay. Mirrors internal/learning/store.go's atomic temp+rename write;
// unlike that store there is exactly one generation live at a time (the
// overlay's own Version field is the history, not the filesystem layout).
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore builds a Store rooted at dir, creating the directory if absent.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("routing: dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("routing: create dir: %w", err)
	}
	return &Store{path: filepath.Join(dir, overlayFileName)}, nil
}

// Load reads the persisted overlay. Returns ok=false, no error, when no
// overlay has ever been saved (fresh install, or routing has never ticked).
func (s *Store) Load() (Overlay, bool, error) {
	if s == nil {
		return Overlay{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Overlay{}, false, nil
		}
		return Overlay{}, false, fmt.Errorf("routing: read overlay: %w", err)
	}
	var o Overlay
	if err := yaml.Unmarshal(data, &o); err != nil {
		return Overlay{}, false, fmt.Errorf("routing: parse overlay: %w", err)
	}
	return o, true, nil
}

// Save atomically persists o, replacing any prior overlay.
func (s *Store) Save(o Overlay) error {
	if s == nil {
		return fmt.Errorf("routing: nil store")
	}
	data, err := yaml.Marshal(o)
	if err != nil {
		return fmt.Errorf("routing: marshal overlay: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fsutil.AtomicWrite(s.path, data); err != nil {
		return fmt.Errorf("routing: write overlay: %w", err)
	}
	return nil
}
