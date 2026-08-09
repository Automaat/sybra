package project

import (
	"context"
	"database/sql"
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
// The lock map is keyed by project id, so an edit to one project never waits
// behind a stalled operation on another. That is the property the file store's
// keyed lock already had.
type SQLStore struct {
	db    *db.DB
	locks sync.Map
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

// Lock serializes a read-modify-write for one project.
func (s *SQLStore) Lock(id string) (func(), error) {
	value, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu, ok := value.(*sync.Mutex)
	if !ok {
		return nil, fmt.Errorf("project store: lock for %q is %T", id, value)
	}
	mu.Lock()
	return func() { mu.Unlock() }, nil
}

// Read returns one record exactly as stored.
func (s *SQLStore) Read(id string) (Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	var doc string
	err := s.db.QueryRowContext(ctx, selectProject, id).Scan(&doc)
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
	if _, err := s.db.ExecContext(ctx, upsertProject,
		p.ID, p.Owner, p.Repo, string(p.Type), string(p.Status), p.ClonePath,
		db.TimeValue(p.UpdatedAt), string(doc)); err != nil {
		return fmt.Errorf("write project: %w", err)
	}
	return nil
}

// List returns every record, ordered by id so the project list is stable.
func (s *SQLStore) List() ([]Project, error) {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectProjects+s.db.OrderText("id"))
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
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return out, nil
}

// Delete removes one record. Removing the clone belongs to the caller that owns
// the disk, which is what deletes it today.
func (s *SQLStore) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), projectQueryTimeout)
	defer cancel()
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
	return err == nil && info.IsDir()
}

var _ Persistence = (*SQLStore)(nil)
