package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/reject"
)

// artifactQueryTimeout bounds every statement. Artifacts are written from the
// agent stream and read from request handlers, neither carrying a context here.
const artifactQueryTimeout = 60 * time.Second

// SQLStore keeps artifact content in the configured database backend.
//
// The bytes live in the row, so an artifact is reachable from a board on
// another machine rather than only from the process holding the directory.
type SQLStore struct {
	db           *db.DB
	maxSizeBytes int64
}

// NewSQLStore returns the database-backed artifact store. maxSizeBytes of zero
// or less means no limit.
func NewSQLStore(database *db.DB, maxSizeBytes int64) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("artifact store needs an open database")
	}
	return &SQLStore{db: database, maxSizeBytes: maxSizeBytes}, nil
}

// created_at is in the conflict update so an overwriting Put stores the timestamp it returns to its caller; a stream Append keeps the original by passing the value it just read back in.
const (
	upsertArtifact = `INSERT INTO task_artifacts (task_id, name, kind, size_bytes, created_at, updated_at, content)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id, name) DO UPDATE SET
			kind = excluded.kind, size_bytes = excluded.size_bytes,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at, content = excluded.content`

	selectArtifact = `SELECT kind, size_bytes, created_at, content
		FROM task_artifacts WHERE task_id = ? AND name = ?`

	selectArtifacts = `SELECT name, kind, size_bytes, created_at FROM task_artifacts WHERE task_id = ? ORDER BY `

	deleteTaskArtifacts = `DELETE FROM task_artifacts WHERE task_id = ?`

	// GROUP BY, not DISTINCT: postgres refuses an ORDER BY expression that is
	// not in a DISTINCT select list, and the collation qualifier makes it one.
	selectArtifactTasks = `SELECT task_id FROM task_artifacts GROUP BY task_id ORDER BY `
)

// Put stores one artifact whole.
func (s *SQLStore) Put(taskID string, a Artifact) (Meta, error) {
	if s == nil {
		return Meta{}, errors.New("artifact store is not configured")
	}
	if !validTaskID.MatchString(taskID) {
		return Meta{}, reject.New("artifact: invalid task id %q", taskID)
	}
	name := a.Name
	if name == "" {
		name = a.Kind.defaultName()
	}
	if !validName.MatchString(name) {
		return Meta{}, reject.New("artifact: invalid artifact name %q", name)
	}
	if err := s.validateSize(int64(len(a.Content))); err != nil {
		return Meta{}, err
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
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, upsertArtifact,
		taskID, name, string(a.Kind), m.Size,
		db.TimeValue(m.CreatedAt), db.TimeValue(m.CreatedAt), a.Content); err != nil {
		return Meta{}, fmt.Errorf("artifact: write: %w", err)
	}
	return m, nil
}

// Append adds one JSON event line to a stream artifact.
//
// Append-only and atomic: the read and the write run in one transaction under a per-stream advisory lock, so a restart mid-stream leaves every line committed before it and never a partial one, and a concurrent append waits rather than overwriting. The file store got both from O_APPEND.
func (s *SQLStore) Append(taskID string, kind Kind, event any) error {
	if s == nil {
		return errors.New("artifact store is not configured")
	}
	if !validTaskID.MatchString(taskID) {
		return reject.New("artifact: invalid task id %q", taskID)
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("artifact: marshal event: %w", err)
	}
	line = append(line, '\n')

	name := kind.defaultName()
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	return s.db.InTxLocked(ctx, appendLockKey(taskID, name), func(tx *sql.Tx) error {
		var existing []byte
		var createdAt int64
		err := tx.QueryRowContext(ctx, s.db.Rebind(
			`SELECT content, created_at FROM task_artifacts WHERE task_id = ? AND name = ?`),
			taskID, name).Scan(&existing, &createdAt)
		now := time.Now().UTC()
		switch {
		case errors.Is(err, sql.ErrNoRows):
			createdAt = db.TimeValue(now)
		case err != nil:
			return fmt.Errorf("artifact: read stream: %w", err)
		}
		// A fresh slice, not an append onto the scanned one: the driver may
		// hand back a buffer it reuses, and extending it in place would write
		// through whatever else is holding it.
		merged := make([]byte, 0, len(existing)+len(line))
		merged = append(merged, existing...)
		merged = append(merged, line...)
		if err := s.validateSize(int64(len(merged))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertArtifact),
			taskID, name, string(kind), int64(len(merged)),
			createdAt, db.TimeValue(now), merged); err != nil {
			return fmt.Errorf("artifact: append: %w", err)
		}
		return nil
	})
}

// Read returns one artifact's content and metadata.
func (s *SQLStore) Read(taskID, name string) ([]byte, Meta, error) {
	if s == nil {
		return nil, Meta{}, errors.New("artifact store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	var (
		kind      string
		size      int64
		createdAt int64
		content   []byte
	)
	err := s.db.QueryRowContext(ctx, selectArtifact, taskID, name).Scan(&kind, &size, &createdAt, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Meta{}, fmt.Errorf("artifact %q not found for task %q", name, taskID)
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("artifact: read: %w", err)
	}
	return content, Meta{
		Name: name, Kind: Kind(kind), TaskID: taskID,
		Size: size, CreatedAt: db.TimeFrom(createdAt),
	}, nil
}

// List returns a task's artifacts ordered by name, which is what the directory
// scan produced.
func (s *SQLStore) List(taskID string) ([]Meta, error) {
	if s == nil {
		return nil, errors.New("artifact store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectArtifacts+s.db.OrderText("name"), taskID)
	if err != nil {
		return nil, fmt.Errorf("artifact: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Meta
	for rows.Next() {
		var (
			m         Meta
			kind      string
			createdAt int64
		)
		if err := rows.Scan(&m.Name, &kind, &m.Size, &createdAt); err != nil {
			return nil, fmt.Errorf("artifact: scan: %w", err)
		}
		m.Kind = Kind(kind)
		m.TaskID = taskID
		m.CreatedAt = db.TimeFrom(createdAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifact: iterate: %w", err)
	}
	return out, nil
}

// Delete removes every artifact belonging to a task.
func (s *SQLStore) Delete(taskID string) error {
	if s == nil {
		return errors.New("artifact store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteTaskArtifacts, taskID); err != nil {
		return fmt.Errorf("artifact: delete: %w", err)
	}
	return nil
}

// ListTaskIDs returns every task with artifacts stored.
func (s *SQLStore) ListTaskIDs() ([]string, error) {
	if s == nil {
		return nil, errors.New("artifact store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), artifactQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectArtifactTasks+s.db.OrderText("task_id"))
	if err != nil {
		return nil, fmt.Errorf("artifact: list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("artifact: scan task id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifact: iterate task ids: %w", err)
	}
	return out, nil
}

// validateSize names the limit, so a refusal says what to change.
func (s *SQLStore) validateSize(size int64) error {
	if s.maxSizeBytes <= 0 {
		return nil
	}
	if size > s.maxSizeBytes {
		return reject.New("artifact is %d bytes, which exceeds the configured limit of %d bytes",
			size, s.maxSizeBytes)
	}
	return nil
}

// appendLockKey serializes concurrent appends to one artifact stream.
//
// A plain transaction is not enough for the row-missing case: under READ COMMITTED two appends both read no row, both insert, and ON CONFLICT DO UPDATE resolves the collision by overwriting, so one agent's line is dropped with no error. The key is per stream rather than one shared key so two tasks' streams still append in parallel.
//
// The digest is split into two 32-bit halves rather than truncated to one, because a stream is named by task and kind and those collide far more readily on a short key than random ids would.
func appendLockKey(taskID, name string) db.LockKey {
	sum := sha256.Sum256([]byte(taskID + "\x00" + name))
	high := int64(binary.BigEndian.Uint32(sum[0:4]))
	low := int64(binary.BigEndian.Uint32(sum[4:8]))
	return db.LockKey(high<<32 | low)
}
