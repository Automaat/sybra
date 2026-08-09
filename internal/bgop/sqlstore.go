package bgop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement this store runs. The tracker saves from paths that carry no context — an operation finishing, a phase advancing — so a stalled backend would otherwise hold one open with nothing able to cancel it.
const queryTimeout = 15 * time.Second

// SQLPersistence keeps background operations in the configured database backend.
//
// One row per operation, with the fields a human scans in an operator query lifted into columns beside the document. Save replaces the whole set in one transaction, so a reader never sees a half-written board.
type SQLPersistence struct {
	db *db.DB
}

// NewSQLPersistence returns the database-backed store.
func NewSQLPersistence(database *db.DB) (*SQLPersistence, error) {
	if database == nil {
		return nil, errors.New("background operation store needs an open database")
	}
	return &SQLPersistence{db: database}, nil
}

const (
	selectBgops = `SELECT doc FROM background_operations ORDER BY started_at, id`

	upsertBgop = `INSERT INTO background_operations (id, kind, status, started_at, doc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			kind = excluded.kind, status = excluded.status,
			started_at = excluded.started_at, doc = excluded.doc`

	deleteAllBgops = `DELETE FROM background_operations`
)

// Load returns the persisted operations, oldest first.
func (p *SQLPersistence) Load() ([]Operation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, selectBgops)
	if err != nil {
		return nil, fmt.Errorf("list background operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ops []Operation
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan background operation: %w", err)
		}
		var op Operation
		if err := json.Unmarshal([]byte(doc), &op); err != nil {
			continue
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background operations: %w", err)
	}
	return ops, nil
}

// Save replaces the persisted set with ops.
//
// The tracker hands over its whole set, and an operation it dropped for age is gone precisely by being absent here, so the old rows go first rather than being diffed against the new ones.
func (p *SQLPersistence) Save(ops []Operation) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	return p.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, p.db.Rebind(deleteAllBgops)); err != nil {
			return fmt.Errorf("clear background operations: %w", err)
		}
		return insertOps(ctx, p.db, tx, ops)
	})
}

func insertOps(ctx context.Context, database *db.DB, tx *sql.Tx, ops []Operation) error {
	for i := range ops {
		doc, err := json.Marshal(ops[i])
		if err != nil {
			return fmt.Errorf("marshal background operation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(upsertBgop),
			ops[i].ID, string(ops[i].Type), string(ops[i].Status),
			db.TimeValue(ops[i].StartedAt), string(doc)); err != nil {
			return fmt.Errorf("write background operation: %w", err)
		}
	}
	return nil
}

var _ Persistence = (*SQLPersistence)(nil)
