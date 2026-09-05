package toolledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement. Store carries no context to cancel one with; see Store.
const queryTimeout = 15 * time.Second

// SQLStore keeps tool-call records in the configured database backend.
type SQLStore struct {
	db *db.DB
}

// NewSQLStore returns the database-backed ledger.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("tool ledger needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const insertLedgerRecord = `INSERT INTO tool_ledger (id, ts, agent_id, task_id, tool, doc)
	VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`

// Log appends one record.
//
// One row, one statement: a crash leaves it written or absent, never a half-line for a later start to repair.
func (l *SQLStore) Log(r Record) error {
	if l == nil {
		return nil
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal tool record: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if _, err := l.db.ExecContext(ctx, insertLedgerRecord,
		RecordID(r), db.TimeValue(r.Timestamp), r.AgentID, r.TaskID, r.Tool, string(doc)); err != nil {
		return fmt.Errorf("write tool record: %w", err)
	}
	return nil
}

// Read returns records in a time window, oldest first. The file ledger had no reader; this exists so the import can be verified and so mining a window does not mean parsing every day-file.
func (l *SQLStore) Read(since, until time.Time) ([]Record, error) {
	if l == nil {
		return nil, nil
	}
	stmt := `SELECT doc FROM tool_ledger WHERE 1 = 1`
	var args []any
	if !since.IsZero() {
		stmt += ` AND ts >= ?`
		args = append(args, db.TimeValue(since))
	}
	if !until.IsZero() {
		stmt += ` AND ts <= ?`
		args = append(args, db.TimeValue(until))
	}
	stmt += ` ORDER BY ts, ` + l.db.OrderText("id")

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := l.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read tool records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan tool record: %w", err)
		}
		var r Record
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool records: %w", err)
	}
	return out, nil
}

// Close is a no-op: the database handle belongs to whoever opened it.
func (l *SQLStore) Close() error { return nil }

// RecordID derives a stable key from the record's own content, so a batch retried after a crash cannot duplicate what it already wrote.
func RecordID(r Record) string {
	doc, err := json.Marshal(r)
	if err != nil {
		doc = []byte(r.Tool + r.AgentID + r.ToolUseID)
	}
	sum := sha256.Sum256(append([]byte(r.Timestamp.UTC().Format(time.RFC3339Nano)+"\x00"), doc...))
	return hex.EncodeToString(sum[:])
}

var _ Store = (*SQLStore)(nil)

func insertRecordTx(ctx context.Context, database *db.DB, tx *sql.Tx, r Record, raw []byte) error {
	if _, err := tx.ExecContext(ctx, database.Rebind(insertLedgerRecord),
		RecordID(r), db.TimeValue(r.Timestamp), r.AgentID, r.TaskID, r.Tool, string(raw)); err != nil {
		return fmt.Errorf("insert tool record: %w", err)
	}
	return nil
}
