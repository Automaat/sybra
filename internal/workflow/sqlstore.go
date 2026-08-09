package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement this store runs.
//
// The Repository methods take no context (see there), so without a deadline a stalled postgres connection would hold a dispatch — and shutdown — open with no way to cancel it. Generous enough that a loaded shared board still answers, short enough that a dead one is reported rather than waited on.
const queryTimeout = 15 * time.Second

// SQLStore persists workflow definitions and their snapshots in the configured database backend.
//
// The YAML document is stored verbatim rather than decomposed into columns. Definitions are read back through the same parser the files went through, so an unmodelled field survives a round trip and a step-shape change needs no migration; the few fields a query filters on are lifted into columns beside the document.
type SQLStore struct {
	db *db.DB
}

// NewSQLStore returns a database-backed workflow repository.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("workflow sql store needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const (
	selectWorkflow = `SELECT doc FROM workflow_definitions WHERE id = ?`

	upsertWorkflow = `INSERT INTO workflow_definitions (id, name, builtin, created_at, updated_at, doc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name, builtin = excluded.builtin,
			created_at = excluded.created_at, updated_at = excluded.updated_at, doc = excluded.doc`

	deleteWorkflow = `DELETE FROM workflow_definitions WHERE id = ?`

	selectWorkflowCreatedAt = `SELECT created_at FROM workflow_definitions WHERE id = ?`

	// A snapshot is keyed by a hash of its own content, so a repeat write carries identical bytes and there is nothing to update.
	insertSnapshot = `INSERT INTO workflow_snapshots (workflow_id, hash, created_at, doc)
		VALUES (?, ?, ?, ?) ON CONFLICT (workflow_id, hash) DO NOTHING`

	selectSnapshot = `SELECT doc FROM workflow_snapshots WHERE workflow_id = ? AND hash = ?`
)

// Dir reports that definitions have no directory under this backend.
func (s *SQLStore) Dir() string { return "" }

// List returns every definition in byte order of its id, which is what the file store's directory listing gave and what the engine breaks priority ties on.
func (s *SQLStore) List() ([]Definition, error) {
	ctx, cancel := s.context()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM workflow_definitions ORDER BY `+s.db.OrderText("id"))
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var defs []Definition
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		def, err := parseDefinition([]byte(doc))
		if err != nil {
			// The file store logs and skips a definition it cannot parse; failing the whole listing here would take every other workflow down with it.
			continue
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflows: %w", err)
	}
	return defs, nil
}

// Get returns one definition by id.
func (s *SQLStore) Get(id string) (Definition, error) {
	ctx, cancel := s.context()
	defer cancel()
	var doc string
	err := s.db.QueryRowContext(ctx, selectWorkflow, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("workflow %q not found: %w", id, err)
	}
	if err != nil {
		return Definition{}, fmt.Errorf("read workflow %q: %w", id, err)
	}
	return parseDefinition([]byte(doc))
}

// Save writes a definition, stamping timestamps the way the file store does: CreatedAt survives the first write, UpdatedAt is always now.
func (s *SQLStore) Save(def Definition) error {
	if def.ID == "" {
		return fmt.Errorf("workflow ID is required")
	}
	ctx, cancel := s.context()
	defer cancel()

	now := db.StoredTime(time.Now().UTC())
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		// Read inside the transaction: two writers would otherwise both find no row, and the later one would reset the definition's age.
		var existing int64
		err := tx.QueryRowContext(ctx, s.db.Rebind(selectWorkflowCreatedAt), def.ID).Scan(&existing)
		switch {
		case err == nil:
			def.CreatedAt = db.TimeFrom(existing)
		case errors.Is(err, sql.ErrNoRows):
			if def.CreatedAt.IsZero() {
				def.CreatedAt = now
			}
		default:
			return fmt.Errorf("read workflow age: %w", err)
		}
		def.UpdatedAt = now

		if vErr := def.Validate(); vErr != nil {
			return fmt.Errorf("validate workflow: %w", vErr)
		}
		doc, mErr := yaml.Marshal(def)
		if mErr != nil {
			return fmt.Errorf("marshal workflow: %w", mErr)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(upsertWorkflow),
			def.ID, def.Name, db.BoolValue(def.Builtin),
			db.TimeValue(def.CreatedAt), db.TimeValue(def.UpdatedAt), string(doc)); err != nil {
			return fmt.Errorf("write workflow: %w", err)
		}
		return nil
	})
}

// Delete removes a definition. Its snapshots stay: a running task references one by hash, and dropping them would strand it mid-workflow.
//
// A missing id is an error, as it is for the file store: the GUI shows what Delete returns, and reporting success on one backend and failure on the other for the same click is worse than either answer.
func (s *SQLStore) Delete(id string) error {
	ctx, cancel := s.context()
	defer cancel()
	res, err := s.db.ExecContext(ctx, deleteWorkflow, id)
	if err != nil {
		return fmt.Errorf("delete workflow %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete workflow %q: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("workflow %s not found", id)
	}
	return nil
}

// SaveSnapshot stores an immutable copy keyed by the definition's semantic hash and returns that hash. An existing snapshot is left exactly as it is.
func (s *SQLStore) SaveSnapshot(def Definition) (string, error) {
	if def.ID == "" {
		return "", fmt.Errorf("workflow ID is required")
	}
	hash, err := def.SemanticHash()
	if err != nil {
		return "", err
	}
	doc, err := yaml.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("marshal workflow snapshot: %w", err)
	}
	ctx, cancel := s.context()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, insertSnapshot,
		def.ID, hash, db.TimeValue(db.StoredTime(time.Now().UTC())), string(doc)); err != nil {
		return "", fmt.Errorf("write workflow snapshot: %w", err)
	}
	return hash, nil
}

// GetSnapshot returns the definition a task was dispatched against.
func (s *SQLStore) GetSnapshot(workflowID, hash string) (Definition, error) {
	if !snapshotHashPattern.MatchString(hash) {
		return Definition{}, fmt.Errorf("invalid workflow snapshot hash %q", hash)
	}
	ctx, cancel := s.context()
	defer cancel()
	var doc string
	err := s.db.QueryRowContext(ctx, selectSnapshot, workflowID, hash).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("workflow snapshot %q/%q not found: %w", workflowID, hash, err)
	}
	if err != nil {
		return Definition{}, fmt.Errorf("read workflow snapshot: %w", err)
	}
	return parseDefinition([]byte(doc))
}

func (s *SQLStore) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), queryTimeout)
}

// parseDefinition decodes and validates a stored workflow document. It is the same decode the file store performs, so a definition read from either backend has passed the same checks.
func parseDefinition(data []byte) (Definition, error) {
	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return Definition{}, fmt.Errorf("unmarshal workflow: %w", err)
	}
	if vErr := def.Validate(); vErr != nil {
		return Definition{}, fmt.Errorf("validate workflow %s: %w", def.ID, vErr)
	}
	return def, nil
}
