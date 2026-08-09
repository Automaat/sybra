package limits

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

// queryTimeout bounds every statement. Persistence carries no context to cancel one with.
const queryTimeout = 15 * time.Second

// SQLPersistence keeps quota state in the configured database backend.
//
// A snapshot is one row per provider and a usage event is one row, so an update writes what changed rather than rewriting the whole document under a cross-process file lock — which is what made every quota update proportional to the length of the history.
type SQLPersistence struct {
	db *db.DB

	// mu serializes critical sections in this process; the advisory lock the
	// transaction takes serializes them against every other process. tx is the
	// transaction a critical section is running in, and is nil outside one.
	mu sync.Mutex
	tx *sql.Tx
}

// LockQuota serializes quota read-modify-writes across processes. Distinct
// from every other advisory key so a quota update never waits on an import.
const LockQuota db.LockKey = 6_215_034_129_004

// Critical runs fn as one atomic read-modify-write.
func (p *SQLPersistence) Critical(fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	return p.db.InTxLocked(ctx, LockQuota, func(tx *sql.Tx) error {
		p.tx = tx
		defer func() { p.tx = nil }()
		return fn()
	})
}

func (p *SQLPersistence) query(ctx context.Context, stmt string, args ...any) (*sql.Rows, error) {
	if p.tx != nil {
		return p.tx.QueryContext(ctx, p.db.Rebind(stmt), args...)
	}
	return p.db.QueryContext(ctx, stmt, args...)
}

// NewSQLPersistence returns the database-backed quota store.
func NewSQLPersistence(database *db.DB) (*SQLPersistence, error) {
	if database == nil {
		return nil, errors.New("quota store needs an open database")
	}
	return &SQLPersistence{db: database}, nil
}

const (
	upsertQuotaSnapshot = `INSERT INTO provider_quota_snapshots (provider, captured_at, doc)
		VALUES (?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET captured_at = excluded.captured_at, doc = excluded.doc`

	insertQuotaUsage = `INSERT INTO provider_quota_usage (id, provider, ts, doc)
		VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`

	deleteMissingSnapshots = `DELETE FROM provider_quota_snapshots`
	deleteUsageBefore      = `DELETE FROM provider_quota_usage WHERE ts < ?`
)

// Load returns the persisted snapshots and usage events, oldest event first.
func (p *SQLPersistence) Load() (map[string]Snapshot, []UsageEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	snapshots := map[string]Snapshot{}
	rows, err := p.query(ctx, `SELECT doc FROM provider_quota_snapshots`)
	if err != nil {
		return nil, nil, fmt.Errorf("read quota snapshots: %w", err)
	}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan quota snapshot: %w", err)
		}
		var s Snapshot
		if err := json.Unmarshal([]byte(doc), &s); err != nil {
			continue
		}
		if s.Provider != "" {
			snapshots[s.Provider] = s
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("iterate quota snapshots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close quota snapshot scan: %w", err)
	}

	eventRows, err := p.query(ctx,
		`SELECT doc FROM provider_quota_usage ORDER BY ts, `+p.db.OrderText("id"))
	if err != nil {
		return nil, nil, fmt.Errorf("read quota usage: %w", err)
	}
	defer func() { _ = eventRows.Close() }()
	var events []UsageEvent
	for eventRows.Next() {
		var doc string
		if err := eventRows.Scan(&doc); err != nil {
			return nil, nil, fmt.Errorf("scan quota usage: %w", err)
		}
		var e UsageEvent
		if err := json.Unmarshal([]byte(doc), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	if err := eventRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate quota usage: %w", err)
	}
	return snapshots, events, nil
}

// Save replaces the persisted state with what the store now holds.
//
// Events are inserted rather than replaced wholesale — their ids are stable, so a repeat is a conflict — and the ones the store dropped for age are deleted by time rather than by absence, which keeps a concurrent writer's newer events from being erased by an older reader's view.
func (p *SQLPersistence) Save(snapshots map[string]Snapshot, events []UsageEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if p.tx != nil {
		return p.save(ctx, p.tx, snapshots, events)
	}
	return p.db.InTx(ctx, func(tx *sql.Tx) error {
		return p.save(ctx, tx, snapshots, events)
	})
}

func (p *SQLPersistence) save(ctx context.Context, tx *sql.Tx, snapshots map[string]Snapshot, events []UsageEvent) error {
	if _, err := tx.ExecContext(ctx, p.db.Rebind(deleteMissingSnapshots)); err != nil {
		return fmt.Errorf("clear quota snapshots: %w", err)
	}
	for provider := range snapshots {
		s := snapshots[provider]
		doc, err := json.Marshal(&s)
		if err != nil {
			return fmt.Errorf("marshal quota snapshot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, p.db.Rebind(upsertQuotaSnapshot),
			s.Provider, db.TimeValue(s.CapturedAt), string(doc)); err != nil {
			return fmt.Errorf("write quota snapshot: %w", err)
		}
	}
	if err := insertEventsTx(ctx, p.db, tx, events); err != nil {
		return err
	}
	// Retention by age, matching the file store's own cutoff.
	cutoff := time.Now().UTC().Add(-eventMaxAge)
	if _, err := tx.ExecContext(ctx, p.db.Rebind(deleteUsageBefore), db.TimeValue(cutoff)); err != nil {
		return fmt.Errorf("prune quota usage: %w", err)
	}
	return nil
}

func insertEventsTx(ctx context.Context, database *db.DB, tx *sql.Tx, events []UsageEvent) error {
	for i := range events {
		if events[i].ID == "" {
			continue
		}
		doc, err := json.Marshal(events[i])
		if err != nil {
			return fmt.Errorf("marshal quota usage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(insertQuotaUsage),
			events[i].ID, events[i].Provider, db.TimeValue(events[i].Timestamp), string(doc)); err != nil {
			return fmt.Errorf("write quota usage: %w", err)
		}
	}
	return nil
}

var _ Persistence = (*SQLPersistence)(nil)
