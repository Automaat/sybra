package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// projectQueryTimeout bounds every statement. The store is called from config
// reloads and GUI actions, neither of which carries a context.
const projectQueryTimeout = 20 * time.Second

// SQLStore keeps project records in the configured database backend.
//
// Only the record moves: the bare clone and its worktrees stay on disk, and the
// record carries the clone's path. That is deliberate — a clone is gigabytes of
// git objects, and the pairing the issue cares about is that a record never
// claims a clone that is not there.
//
// Lock takes a cross-process advisory lock keyed by the project id, not merely
// a mutex in this process: on a shared board two instances editing one project
// would otherwise interleave read-modify-write and lose an edit. The key is
// derived from the id, so an edit to one project never waits behind a stalled
// operation on another — the property the file store's keyed lock had.
type SQLStore struct {
	db    *db.DB
	local sync.Map

	mu sync.Mutex
	tx *sql.Tx
}

// lockKeyFor derives the advisory key for one project. Distinct from every
// named LockKey by construction: those are small constants and this is a hash.
func lockKeyFor(id string) db.LockKey {
	sum := sha256.Sum256([]byte("projects:" + id))
	// Built from two 32-bit halves with the high one capped to 31 bits, so the
	// result provably fits a signed 64-bit advisory key. Masking a full uint64
	// would do the same but leaves the compiler and the overflow check unable
	// to see it.
	high := int64(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	low := int64(binary.BigEndian.Uint32(sum[4:8]))
	return db.LockKey(high<<32 | low)
}

// NewSQLStore returns the database-backed project records.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("project store needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const (
	selectProject = `SELECT doc FROM projects WHERE id = ?`

	upsertProject = `INSERT INTO projects (id, owner, repo, type, status, clone_path, updated_at, doc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			owner = excluded.owner, repo = excluded.repo, type = excluded.type,
			status = excluded.status, clone_path = excluded.clone_path,
			updated_at = excluded.updated_at, doc = excluded.doc`

	deleteProject = `DELETE FROM projects WHERE id = ?`

	selectProjects = `SELECT doc FROM projects ORDER BY `
)

// Lock serializes a read-modify-write for one project, across processes.
//
// The returned release commits the transaction the lock is held in; Read and
// Write called inside it run in that same transaction, so the cycle is atomic
// against every other instance rather than only against this one's goroutines.
func (s *SQLStore) Lock(id string) (func(), error) {
	value, _ := s.local.LoadOrStore(id, &sync.Mutex{})
	localMu, ok := value.(*sync.Mutex)
	if !ok {
		return nil, fmt.Errorf("project store: lock for %q is %T", id, value)
	}
	localMu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		cancel()
		localMu.Unlock()
		return nil, fmt.Errorf("project store: begin: %w", err)
	}
	if s.db.Dialect() == db.Postgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(lockKeyFor(id))); err != nil {
			_ = tx.Rollback()
			cancel()
			localMu.Unlock()
			return nil, fmt.Errorf("project store: lock: %w", err)
		}
	}
	s.mu.Lock()
	s.tx = tx
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		s.tx = nil
		s.mu.Unlock()
		if err := tx.Commit(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			_ = tx.Rollback()
		}
		cancel()
		localMu.Unlock()
	}, nil
}

// active returns the transaction a locked cycle is running in, or nil.
func (s *SQLStore) active() *sql.Tx {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tx
}

// Read returns one record exactly as stored.
func (s *SQLStore) Read(id string) (Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	var doc string
	var err error
	if tx := s.active(); tx != nil {
		err = tx.QueryRowContext(ctx, s.db.Rebind(selectProject), id).Scan(&doc)
	} else {
		err = s.db.QueryRowContext(ctx, selectProject, id).Scan(&doc)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("read project %s: %w", id, ErrProjectNotRegistered)
	}
	if err != nil {
		return Project{}, fmt.Errorf("read project: %w", err)
	}
	var p Project
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		return Project{}, fmt.Errorf("parse project: %w", err)
	}
	return p, nil
}

// Write stores one record.
func (s *SQLStore) Write(p Project) error {
	if p.ID == "" {
		return errors.New("project store: record has no id")
	}
	doc, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal project: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	args := []any{p.ID, p.Owner, p.Repo, string(p.Type), string(p.Status), p.ClonePath,
		db.TimeValue(p.UpdatedAt), string(doc)}
	if tx := s.active(); tx != nil {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertProject), args...); err != nil {
			return fmt.Errorf("write project: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx, upsertProject, args...); err != nil {
		return fmt.Errorf("write project: %w", err)
	}
	return nil
}

// List returns every record, ordered by id so the project list is stable.
func (s *SQLStore) List() ([]Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	query := selectProjects + s.db.OrderText("id")
	var rows *sql.Rows
	var err error
	if tx := s.active(); tx != nil {
		rows, err = tx.QueryContext(ctx, s.db.Rebind(query))
	} else {
		rows, err = s.db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Project
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		var p Project
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			// Skipped as the file store skips an unparseable file, rather than
			// costing the caller every other project.
			continue
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", db.Contended(err))
	}
	return out, nil
}

// Delete removes one record. Removing the clone belongs to the caller that owns
// the disk, which is what deletes it today.
func (s *SQLStore) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	if tx := s.active(); tx != nil {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteProject), id); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx, deleteProject, id); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return nil
}

// CloneUsable reports whether a record's clone is actually on disk.
//
// The record and the clone are updated independently, so a crash between them
// can leave a project marked ready with nothing behind it. A caller about to
// use the clone asks first rather than discovering it mid-operation.
func CloneUsable(p Project) bool {
	if p.ClonePath == "" {
		return false
	}
	info, err := os.Stat(p.ClonePath)
	if err != nil {
		return false
	}
	return info.IsDir()
}

var _ Persistence = (*SQLStore)(nil)
