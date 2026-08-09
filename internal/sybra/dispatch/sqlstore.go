package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// LockLedger serializes admission read-modify-writes across processes. Distinct from every other advisory key so a dispatch never waits on an import.
const LockLedger db.LockKey = 6_215_034_129_005

// ledgerName keys this ledger's revision row. One ledger exists today; the column is there so a second never has to migrate.
const ledgerName = "attempt_leases"

// Records are stored as the YAML the file document already used, so the import
// and this store agree by construction and Record needs no second tag set.
//
// SQLPersistence keeps the admission ledger in the configured database backend.
//
// A lease is a row, so reclaiming expired ones is a predicate rather than a scan, and a read for one task touches only its rows. The whole read-modify-write runs in one advisory-locked transaction: losing an update here releases work that is still running, and a process killed mid-update leaves the previous state rather than a partial one — which the file document could not promise, because its lock died with the process holding it.
type SQLPersistence struct {
	db *db.DB

	// mu serializes critical sections in this process; the advisory lock the
	// transaction takes serializes them against every other process. tx is the
	// transaction a critical section runs in, and is nil outside one.
	mu sync.Mutex
	tx *sql.Tx
}

// NewSQLPersistence returns the database-backed ledger.
func NewSQLPersistence(database *db.DB) (*SQLPersistence, error) {
	if database == nil {
		return nil, errors.New("attempt lease store needs an open database")
	}
	return &SQLPersistence{db: database}, nil
}

const (
	selectLeases = `SELECT doc FROM attempt_leases ORDER BY `

	upsertLease = `INSERT INTO attempt_leases (id, task_id, provider, status, expires_at, doc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			task_id = excluded.task_id, provider = excluded.provider,
			status = excluded.status, expires_at = excluded.expires_at, doc = excluded.doc`

	deleteAllLeases = `DELETE FROM attempt_leases`

	selectRevision = `SELECT revision FROM ledger_revisions WHERE ledger = ?`

	upsertRevision = `INSERT INTO ledger_revisions (ledger, revision) VALUES (?, ?)
		ON CONFLICT (ledger) DO UPDATE SET revision = excluded.revision`
)

// Critical runs fn as one atomic read-modify-write.
func (p *SQLPersistence) Critical(ctx context.Context, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db.InTxLocked(ctx, LockLedger, func(tx *sql.Tx) error {
		p.tx = tx
		defer func() { p.tx = nil }()
		return fn()
	})
}

// Load returns the ledger, ordered by lease id so a restart sees what the last
// start saw. A table has no natural order, and the order is byte-wise because
// the file document's own slice order was.
func (p *SQLPersistence) Load(ctx context.Context) (diskState, error) {
	rows, err := p.query(ctx, selectLeases+p.db.OrderText("id"))
	if err != nil {
		return diskState{}, fmt.Errorf("read attempt lease store: %w", err)
	}
	defer func() { _ = rows.Close() }()

	s := diskState{SchemaVersion: 1}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return diskState{}, fmt.Errorf("scan attempt lease: %w", err)
		}
		var rec Record
		if err := yaml.Unmarshal([]byte(doc), &rec); err != nil {
			// Refused rather than skipped: this ledger decides whether a task
			// may dispatch, so a lease this build cannot read must not silently
			// become headroom for a second agent on the same work.
			return diskState{}, fmt.Errorf("decode attempt lease: %w", err)
		}
		s.Leases = append(s.Leases, rec)
	}
	if err := rows.Err(); err != nil {
		return diskState{}, fmt.Errorf("iterate attempt leases: %w", err)
	}

	revision, err := p.revision(ctx)
	if err != nil {
		return diskState{}, err
	}
	s.Revision = revision
	return s, nil
}

// Save replaces the ledger with s.
//
// Called only from inside Critical, so the delete and the inserts are one
// transaction under the advisory lock: no other writer observes the gap, and a
// process killed here commits neither half.
func (p *SQLPersistence) Save(ctx context.Context, s diskState) error {
	if p.tx == nil {
		return errors.New("attempt lease store: Save outside a critical section")
	}
	if _, err := p.tx.ExecContext(ctx, p.db.Rebind(deleteAllLeases)); err != nil {
		return fmt.Errorf("clear attempt leases: %w", err)
	}
	for i := range s.Leases {
		rec := &s.Leases[i]
		doc, err := yaml.Marshal(rec)
		if err != nil {
			return fmt.Errorf("encode attempt lease: %w", err)
		}
		if _, err := p.tx.ExecContext(ctx, p.db.Rebind(upsertLease),
			rec.ID, rec.Intent.TaskID, rec.Intent.Provider, string(rec.Status),
			db.TimeValue(rec.ExpiresAt), string(doc)); err != nil {
			return fmt.Errorf("write attempt lease: %w", err)
		}
	}
	if _, err := p.tx.ExecContext(ctx, p.db.Rebind(upsertRevision), ledgerName, revisionValue(s.Revision)); err != nil {
		return fmt.Errorf("write ledger revision: %w", err)
	}
	return nil
}

func (p *SQLPersistence) revision(ctx context.Context) (uint64, error) {
	var revision int64
	var err error
	if p.tx != nil {
		err = p.tx.QueryRowContext(ctx, p.db.Rebind(selectRevision), ledgerName).Scan(&revision)
	} else {
		err = p.db.QueryRowContext(ctx, selectRevision, ledgerName).Scan(&revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read ledger revision: %w", err)
	}
	if revision < 0 {
		// Only a hand-edited row can be negative, and treating it as a huge
		// unsigned revision would make every later write look stale.
		return 0, fmt.Errorf("ledger revision %d is negative", revision)
	}
	return uint64(revision), nil
}

func (p *SQLPersistence) query(ctx context.Context, stmt string, args ...any) (*sql.Rows, error) {
	if p.tx != nil {
		return p.tx.QueryContext(ctx, p.db.Rebind(stmt), args...)
	}
	return p.db.QueryContext(ctx, stmt, args...)
}

var _ Persistence = (*SQLPersistence)(nil)

// revisionValue narrows the ledger revision for a signed column.
//
// The counter increments once per change and would need billions of years of
// dispatches to reach this, but a silent wrap would make a fresh ledger look
// newer than a live one, so it saturates instead.
func revisionValue(revision uint64) int64 {
	if revision > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(revision)
}
