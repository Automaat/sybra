package bgop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement this store runs. The tracker saves from paths that carry no context — an operation finishing, a phase advancing — so a stalled backend would otherwise hold one open with nothing able to cancel it.
const queryTimeout = 15 * time.Second

// SQLPersistence keeps background operations in the configured database backend.
//
// One row per operation, with the fields a human scans in an operator query lifted into columns beside the document.
//
// Rows are scoped to an owner, and an instance reads and replaces only its own. A postgres board is shared by several machines by design, and each tracker hands over its whole set on every change — so without the scope, one machine starting an operation would delete every operation the others were running. It also matches what the file backend did, where each machine kept its own document.
type SQLPersistence struct {
	db    *db.DB
	owner string
}

// NewSQLPersistence returns the database-backed store for one instance.
//
// owner must be stable across restarts for a given instance, or that instance abandons its own rows on every start and never restores them. It must differ between instances sharing a board.
func NewSQLPersistence(database *db.DB, owner string) (*SQLPersistence, error) {
	if database == nil {
		return nil, errors.New("background operation store needs an open database")
	}
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("background operation store needs an owner")
	}
	return &SQLPersistence{db: database, owner: owner}, nil
}

const (
	upsertBgop = `INSERT INTO background_operations (id, owner, kind, status, started_at, doc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			owner = excluded.owner, kind = excluded.kind, status = excluded.status,
			started_at = excluded.started_at, doc = excluded.doc`

	deleteOwnedBgops = `DELETE FROM background_operations WHERE owner = ?`
)

// Load returns the persisted operations, oldest first.
func (p *SQLPersistence) Load() ([]Operation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := p.db.QueryContext(ctx,
		`SELECT doc FROM background_operations WHERE owner = ? ORDER BY started_at, `+p.db.OrderText("id"), p.owner)
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
		if _, err := tx.ExecContext(ctx, p.db.Rebind(deleteOwnedBgops), p.owner); err != nil {
			return fmt.Errorf("clear background operations: %w", err)
		}
		return insertOps(ctx, p.db, tx, p.owner, ops)
	})
}

func insertOps(ctx context.Context, database *db.DB, tx *sql.Tx, owner string, ops []Operation) error {
	for i := range ops {
		doc, err := json.Marshal(ops[i])
		if err != nil {
			return fmt.Errorf("marshal background operation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(upsertBgop),
			ops[i].ID, owner, string(ops[i].Type), string(ops[i].Status),
			db.TimeValue(ops[i].StartedAt), string(doc)); err != nil {
			return fmt.Errorf("write background operation: %w", err)
		}
	}
	return nil
}

var _ Persistence = (*SQLPersistence)(nil)
