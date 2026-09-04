package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// LockIncidents serializes incident read-modify-writes across processes.
const LockIncidents db.LockKey = 6_215_034_129_006

// incidentQueryTimeout bounds a held lock. The file lock it replaces had the
// same ceiling, and an incident cycle that cannot finish must release rather
// than hold every other instance's monitor pass behind it.
const incidentQueryTimeout = 30 * time.Second

// SQLIncidentStore keeps the incident ledger in the configured database backend.
//
// Lock begins a transaction holding an advisory lock and its release commits,
// so a whole observe-or-reconcile cycle is atomic against every other instance
// — and a process killed mid-cycle leaves the previous state rather than an
// incident recorded without the remediation it was recorded for.
type SQLIncidentStore struct {
	db *db.DB

	mu     sync.Mutex
	tx     *db.WriteTx
	failed bool
}

// NewSQLIncidentStore returns the database-backed incident ledger.
func NewSQLIncidentStore(database *db.DB) (*SQLIncidentStore, error) {
	if database == nil {
		return nil, errors.New("incident store needs an open database")
	}
	return &SQLIncidentStore{db: database}, nil
}

const (
	selectIncident = `SELECT doc FROM monitor_incidents WHERE fingerprint = ?`

	upsertIncident = `INSERT INTO monitor_incidents (fingerprint, state, failure_code, last_seen, doc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			state = excluded.state, failure_code = excluded.failure_code,
			last_seen = excluded.last_seen, doc = excluded.doc`

	selectIncidents = `SELECT doc FROM monitor_incidents ORDER BY `
)

// Lock begins the transaction a read-modify-write runs in.
//
// The returned release commits it, unless a Save inside the cycle failed — then
// it rolls back, so a cycle that could not finish leaves the ledger at its
// previous value rather than committing the half of it that worked. Nothing
// else may touch the store until release is called, which the process-local
// mutex enforces here and the advisory lock enforces against every other
// process.
func (s *SQLIncidentStore) Lock() (func() error, error) {
	s.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), incidentQueryTimeout)

	tx, err := s.db.BeginWriteTx(ctx)
	if err != nil {
		cancel()
		s.mu.Unlock()
		return nil, fmt.Errorf("incident store: begin: %w", err)
	}
	if s.db.Dialect() == db.Postgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(LockIncidents)); err != nil {
			_ = tx.Rollback()
			cancel()
			s.mu.Unlock()
			return nil, fmt.Errorf("incident store: lock: %w", err)
		}
	}
	s.tx = tx
	s.failed = false

	return func() error {
		failed := s.failed
		defer func() {
			s.tx = nil
			s.failed = false
			cancel()
			s.mu.Unlock()
		}()
		if failed {
			if err := tx.Rollback(); err != nil {
				return fmt.Errorf("incident store: roll back: %w", err)
			}
			return errors.New("incident store: cycle rolled back after a failed write")
		}
		return tx.Commit()
	}, nil
}

// Load returns one incident by fingerprint.
func (s *SQLIncidentStore) Load(fingerprint string) (Incident, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), incidentQueryTimeout)
	defer cancel()
	var doc string
	var err error
	if s.tx != nil {
		err = s.tx.QueryRowContext(ctx, s.db.Rebind(selectIncident), fingerprint).Scan(&doc)
	} else {
		err = s.db.QueryRowContext(ctx, selectIncident, fingerprint).Scan(&doc)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("incident store: read: %w", err)
	}
	var in Incident
	if err := yaml.Unmarshal([]byte(doc), &in); err != nil {
		return Incident{}, false, fmt.Errorf("incident store: decode: %w", err)
	}
	return in, true, nil
}

// Save writes one incident.
func (s *SQLIncidentStore) Save(in Incident) error {
	if s.tx == nil {
		return errors.New("incident store: Save outside a locked cycle")
	}
	doc, err := yaml.Marshal(in)
	if err != nil {
		return fmt.Errorf("incident store: encode: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), incidentQueryTimeout)
	defer cancel()
	if _, err := s.tx.ExecContext(ctx, s.db.Rebind(upsertIncident),
		in.Fingerprint, string(in.State), in.FailureCode, db.TimeValue(in.LastSeen), string(doc)); err != nil {
		s.failed = true
		return fmt.Errorf("incident store: write: %w", err)
	}
	return nil
}

// List returns every incident, ordered by fingerprint so a reconcile pass sees
// them in the same order every time.
func (s *SQLIncidentStore) List() ([]Incident, error) {
	ctx, cancel := context.WithTimeout(context.Background(), incidentQueryTimeout)
	defer cancel()
	stmt := selectIncidents + s.db.OrderText("fingerprint")
	var rows *sql.Rows
	var err error
	if s.tx != nil {
		rows, err = s.tx.QueryContext(ctx, s.db.Rebind(stmt))
	} else {
		rows, err = s.db.QueryContext(ctx, stmt)
	}
	if err != nil {
		return nil, fmt.Errorf("incident store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Incident
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("incident store: scan: %w", err)
		}
		var in Incident
		if err := yaml.Unmarshal([]byte(doc), &in); err != nil {
			return nil, fmt.Errorf("incident store: decode: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incident store: iterate: %w", err)
	}
	return out, nil
}

var _ IncidentPersistence = (*SQLIncidentStore)(nil)
