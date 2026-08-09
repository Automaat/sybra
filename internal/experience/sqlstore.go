package experience

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/fsutil"
)

// SQLStore persists advisory records in the configured database backend.
//
// The record is stored as the JSON document the file store already wrote, with
// only the three fields a query needs lifted into columns. These records are
// advisory context for triage and planning, never a decision gate, so nothing
// reads an individual field in SQL and a schema per field would buy a
// migration for every addition to Record.
type SQLStore struct {
	db            *db.DB
	maxPerProject int
}

// NewSQLStore returns a database-backed advisory memory.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("experience sql store needs an open database")
	}
	return &SQLStore{db: database, maxPerProject: defaultMaxPerProject}, nil
}

const (
	upsertExperienceSQLite = `INSERT INTO experience_records (project_key, record_id, created_at, doc)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (project_key, record_id) DO UPDATE SET created_at = excluded.created_at, doc = excluded.doc`

	deleteExperienceProject = `DELETE FROM experience_records WHERE project_key = ?`

	deleteExperienceRecord = `DELETE FROM experience_records WHERE project_key = ? AND record_id = ?`
)

// Put writes one record and enforces the per-project cap, both in one
// transaction so a reader never sees the write without the eviction.
func (s *SQLStore) Put(projectID string, rec Record) error {
	if s == nil {
		return nil
	}
	key, err := fsutil.ProjectKeyDir(projectID)
	if err != nil {
		return err
	}
	recordID, err := sanitizeRecordID(rec.TaskID)
	if err != nil {
		return err
	}
	doc, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal experience record: %w", err)
	}
	ctx, cancel := s.context()
	defer cancel()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(s.upsertSQL()),
			key, recordID, db.TimeValue(rec.CreatedAt), string(doc)); err != nil {
			return fmt.Errorf("write experience record: %w", err)
		}
		return s.enforceCapTx(ctx, tx, key)
	})
}

// Query returns up to limit records for a project, newest first.
func (s *SQLStore) Query(projectID string, limit int) ([]Record, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	key, err := fsutil.ProjectKeyDir(projectID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := s.context()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, s.selectSQL(), key, limit)
	if err != nil {
		return nil, fmt.Errorf("query experience records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan experience record: %w", err)
		}
		var rec Record
		if err := json.Unmarshal([]byte(doc), &rec); err != nil {
			// Matches the file store, which skips a record it cannot parse
			// rather than failing the query: this is advisory context, and one
			// unreadable row must not cost an agent all of it.
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate experience records: %w", err)
	}
	return out, nil
}

// Delete drops every record for a project.
func (s *SQLStore) Delete(projectID string) error {
	if s == nil {
		return nil
	}
	key, err := fsutil.ProjectKeyDir(projectID)
	if err != nil {
		return err
	}
	ctx, cancel := s.context()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteExperienceProject, key); err != nil {
		return fmt.Errorf("delete project experience records: %w", err)
	}
	return nil
}

func (s *SQLStore) upsertSQL() string {
	// Postgres spells the conflict target the same way; the statement is
	// shared because both engines accept this form.
	return upsertExperienceSQLite
}

func (s *SQLStore) enforceCapTx(ctx context.Context, tx *sql.Tx, projectKey string) error {
	maxRecords := s.maxPerProject
	if maxRecords <= 0 {
		maxRecords = defaultMaxPerProject
	}
	rows, err := tx.QueryContext(ctx, s.db.Rebind(s.overCapSQL()), projectKey, maxRecords)
	if err != nil {
		return fmt.Errorf("find experience records over cap: %w", err)
	}
	var doomed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan experience record id: %w", err)
		}
		doomed = append(doomed, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate experience records over cap: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close experience record scan: %w", err)
	}
	for _, id := range doomed {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(deleteExperienceRecord), projectKey, id); err != nil {
			return fmt.Errorf("evict experience record: %w", err)
		}
	}
	return nil
}

// selectSQL is the ordered read. Newest first, ties broken by record id in byte
// order so both engines return what the file store's own sort returned.
func (s *SQLStore) selectSQL() string {
	return `SELECT doc FROM experience_records WHERE project_key = ? ORDER BY ` + s.order() + ` LIMIT ?`
}

// overCapSQL finds the records beyond the per-project cap, which the file store
// enforces by deleting the oldest files. SQLite spells an offset without a
// limit as "LIMIT -1 OFFSET n"; postgres rejects that and takes a bare OFFSET.
func (s *SQLStore) overCapSQL() string {
	stmt := `SELECT record_id FROM experience_records WHERE project_key = ? ORDER BY ` + s.order()
	if s.db.Dialect() == db.Postgres {
		return stmt + ` OFFSET ?`
	}
	return stmt + ` LIMIT -1 OFFSET ?`
}

func (s *SQLStore) order() string {
	return `created_at DESC, ` + s.db.OrderText("record_id") + ` ASC`
}

// queryTimeout bounds every statement this store runs, because Repository carries no context to cancel one with. See Repository.
const queryTimeout = 15 * time.Second

func (s *SQLStore) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), queryTimeout)
}
