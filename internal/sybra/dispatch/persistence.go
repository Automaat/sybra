package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/fsutil"
)

// Persistence is where the admission ledger survives a restart. Two implementations exist: the YAML document and the database-backed SQLPersistence, selected by config.Database.Backend.
//
// Critical wraps a whole read-modify-write. Losing one costs more here than anywhere else in the board: a lost lease releases work that is still running, so a second agent starts on a task another is already holding.
type Persistence interface {
	Load(ctx context.Context) (diskState, error)
	Save(ctx context.Context, s diskState) error
	Critical(ctx context.Context, fn func() error) error
}

// filePersistence keeps the ledger in one YAML document, serialized across processes by an advisory lock on that document.
type filePersistence struct {
	path string
}

func newFilePersistence(path string) *filePersistence {
	return &filePersistence{path: path}
}

// Critical takes the cross-process lock for the duration of fn.
//
// The lock only holds while this process lives, which is the weakness the
// database backend exists to remove: a process killed here leaves the document
// unlocked and, if it died between the read and the write, stale.
func (p *filePersistence) Critical(ctx context.Context, fn func() error) error {
	unlock, err := fsutil.LockFileContext(ctx, p.path)
	if err != nil {
		return fmt.Errorf("lock attempt lease store: %w", err)
	}
	defer func() { _ = unlock() }()
	return fn()
}

func (p *filePersistence) Load(context.Context) (diskState, error) {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return diskState{SchemaVersion: 1}, nil
	}
	if err != nil {
		return diskState{}, fmt.Errorf("read attempt lease store: %w", err)
	}
	var s diskState
	if err := yaml.Unmarshal(data, &s); err != nil {
		return diskState{}, fmt.Errorf("decode attempt lease store: %w", err)
	}
	if s.SchemaVersion != 1 {
		return diskState{}, fmt.Errorf("unsupported attempt lease schema %d", s.SchemaVersion)
	}
	return s, nil
}

func (p *filePersistence) Save(_ context.Context, s diskState) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode attempt lease store: %w", err)
	}
	if err := fsutil.AtomicWrite(p.path, data); err != nil {
		return fmt.Errorf("persist attempt lease store: %w", err)
	}
	return nil
}

var _ Persistence = (*filePersistence)(nil)
