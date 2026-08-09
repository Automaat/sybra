package cleanup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// LockProtected serializes protected-findings read-modify-writes across
// processes.
const LockProtected db.LockKey = 6_215_034_129_007

// protectedQueryTimeout bounds a held lock. A cycle that cannot finish must
// release rather than hold every other instance's cleanup pass behind it.
const protectedQueryTimeout = 30 * time.Second

// SQLProtectedStore keeps protected findings in the configured database
// backend.
//
// Lock begins a transaction holding an advisory lock and its release commits,
// so a whole observe-or-resolve cycle is atomic against every other instance. A
// process killed inside one leaves the previous state: losing the record that a
// path is protected is what lets a later cleanup pass delete it.
type SQLProtectedStore struct {
	db *db.DB

	mu sync.Mutex
	tx *sql.Tx
}

// NewSQLProtectedStore returns the database-backed findings ledger.
func NewSQLProtectedStore(database *db.DB) (*SQLProtectedStore, error) {
	if database == nil {
		return nil, errors.New("protected findings store needs an open database")
	}
	return &SQLProtectedStore{db: database}, nil
}

const (
	// One statement per dialect rather than a concatenation, so nothing in this
	// file builds SQL from a value at runtime.
	selectProtectedByKey  = `SELECT doc FROM cleanup_protected ORDER BY key`
	selectProtectedByKeyC = `SELECT doc FROM cleanup_protected ORDER BY key COLLATE "C"`

	// Both placeholder styles spelled out rather than rebound at the call, so
	// every statement this file executes is a constant.
	upsertProtected = `INSERT INTO cleanup_protected (key, kind, recorded_at, doc)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			kind = excluded.kind, recorded_at = excluded.recorded_at, doc = excluded.doc`

	upsertProtectedPG = `INSERT INTO cleanup_protected (key, kind, recorded_at, doc)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET
			kind = excluded.kind, recorded_at = excluded.recorded_at, doc = excluded.doc`

	deleteAllProtected = `DELETE FROM cleanup_protected`
)

// Lock begins the transaction a read-modify-write runs in. The returned release
// commits it.
func (s *SQLProtectedStore) Lock() (func(), error) {
	s.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), protectedQueryTimeout)

	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		cancel()
		s.mu.Unlock()
		return nil, fmt.Errorf("protected findings: begin: %w", err)
	}
	if s.db.Dialect() == db.Postgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(LockProtected)); err != nil {
			_ = tx.Rollback()
			cancel()
			s.mu.Unlock()
			return nil, fmt.Errorf("protected findings: lock: %w", err)
		}
	}
	s.tx = tx

	return func() {
		defer func() {
			s.tx = nil
			cancel()
			s.mu.Unlock()
		}()
		if err := tx.Commit(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			// The release carries no error by contract, so a commit that fails
			// has nowhere to report. Rolling back keeps the ledger at its
			// previous value rather than leaving a half-applied cycle.
			_ = tx.Rollback()
		}
	}, nil
}

// Read returns the whole ledger, ordered by key so a pass sees findings in the
// same order every time — which is what the document's own sorted slice gave.
func (s *SQLProtectedStore) Read() (protectedFile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), protectedQueryTimeout)
	defer cancel()
	var rows *sql.Rows
	var err error
	if s.db.Dialect() == db.Postgres {
		if s.tx != nil {
			rows, err = s.tx.QueryContext(ctx, selectProtectedByKeyC)
		} else {
			rows, err = s.db.QueryContext(ctx, selectProtectedByKeyC)
		}
	} else {
		if s.tx != nil {
			rows, err = s.tx.QueryContext(ctx, selectProtectedByKey)
		} else {
			rows, err = s.db.QueryContext(ctx, selectProtectedByKey)
		}
	}
	if err != nil {
		return protectedFile{}, fmt.Errorf("protected findings: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rec protectedFile
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return protectedFile{}, fmt.Errorf("protected findings: scan: %w", err)
		}
		var f Finding
		if err := json.Unmarshal([]byte(doc), &f); err != nil {
			return protectedFile{}, fmt.Errorf("protected findings: decode: %w", err)
		}
		rec.Findings = append(rec.Findings, f)
	}
	if err := rows.Err(); err != nil {
		return protectedFile{}, fmt.Errorf("protected findings: iterate: %w", err)
	}
	return rec, nil
}

// Write replaces the ledger.
//
// Called only inside a locked cycle, so the delete and the inserts are one
// transaction: no other instance observes the gap, and a process killed here
// commits neither half.
func (s *SQLProtectedStore) Write(rec protectedFile) error {
	if s.tx == nil {
		return errors.New("protected findings: Write outside a locked cycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), protectedQueryTimeout)
	defer cancel()
	if _, err := s.tx.ExecContext(ctx, deleteAllProtected); err != nil {
		return fmt.Errorf("protected findings: clear: %w", err)
	}
	for i := range rec.Findings {
		f := &rec.Findings[i]
		doc, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("protected findings: encode: %w", err)
		}
		if _, err := s.tx.ExecContext(ctx, upsertProtectedStmt(s.db),
			f.ID, string(f.Kind), db.TimeValue(f.FirstSeenAt), string(doc)); err != nil {
			return fmt.Errorf("protected findings: write: %w", err)
		}
	}
	return nil
}

var _ ProtectedPersistence = (*SQLProtectedStore)(nil)

// upsertProtectedStmt returns the finding upsert for this dialect.
func upsertProtectedStmt(d *db.DB) string {
	if d.Dialect() == db.Postgres {
		return upsertProtectedPG
	}
	return upsertProtected
}
