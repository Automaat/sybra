package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement. Store carries no context to cancel one with; see Store.
const queryTimeout = 15 * time.Second

// SQLStore keeps the audit trail in the configured database backend.
//
// A read filtered by time or task is a WHERE against an index, so it touches only matching rows. The file trail had to open every day-file in range and decode every line to answer the same question, which grew with the length of the history rather than with the question.
type SQLStore struct {
	db *db.DB
}

// NewSQLStore returns the database-backed trail.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("audit store needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const insertAuditEvent = `INSERT INTO audit_events (id, ts, event_type, task_id, agent_id, doc)
	VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`

const deleteAuditBefore = `DELETE FROM audit_events WHERE ts < ?`

// Log appends one event.
//
// One row, one statement: a crash leaves the row written or absent, never half of it, so nothing has to repair a truncated tail on the next start.
func (s *SQLStore) Log(e Event) error {
	if s == nil {
		return nil
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	doc, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, insertAuditEvent,
		EventID(e), db.TimeValue(e.Timestamp), e.Type, e.TaskID, e.AgentID, string(doc)); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

// Read returns the events matching q, oldest first, as the file trail did.
func (s *SQLStore) Read(q Query) ([]Event, error) {
	if s == nil {
		return nil, nil
	}
	// An unbounded query is not what the file trail answered: with no window it
	// formats both bounds as year one, matches no day-file, and returns
	// nothing. Returning the whole table instead would load an unbounded
	// history into memory for a caller that forgot a window.
	if q.Since.IsZero() && q.Until.IsZero() && strings.TrimSpace(q.Type) == "" && strings.TrimSpace(q.TaskID) == "" {
		return nil, nil
	}
	stmt := `SELECT doc FROM audit_events WHERE 1 = 1`
	var args []any
	if !q.Since.IsZero() {
		stmt += ` AND ts >= ?`
		args = append(args, db.TimeValue(q.Since))
	}
	if !q.Until.IsZero() {
		stmt += ` AND ts <= ?`
		args = append(args, db.TimeValue(q.Until))
	}
	if prefix := strings.TrimSpace(q.Type); prefix != "" {
		// A prefix, not an equality: the file reader matches with HasPrefix, the
		// CLI flag is documented as a prefix, and the statistics backfill asks
		// for "agent." expecting every agent event. Equality returned none of
		// them.
		//
		// Expressed as a range rather than LIKE so the index is usable and no
		// wildcard in the caller's string is interpreted.
		// Collated, like the ordering is: a range comparison follows the
		// server's collation too, and the deploy target's en_US.UTF-8 ignores
		// the dot in "agent." at the primary level — so the bounds admit and
		// exclude the wrong rows. Byte order is what HasPrefix means.
		col := s.db.OrderText("event_type")
		stmt += ` AND ` + col + ` >= ? AND ` + col + ` < ?`
		args = append(args, prefix, prefixUpperBound(prefix))
	}
	if strings.TrimSpace(q.TaskID) != "" {
		stmt += ` AND task_id = ?`
		args = append(args, q.TaskID)
	}
	stmt += ` ORDER BY ts, ` + s.db.OrderText("id")

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		var e Event
		if err := json.Unmarshal([]byte(doc), &e); err != nil {
			// The file reader skips a line it cannot decode rather than failing
			// the query; one unreadable row must not cost a report all of it.
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return out, nil
}

// prefixUpperBound returns the first string greater than every string starting
// with prefix, so a range scan expresses "has this prefix" exactly.
//
// A prefix whose bytes are all 0xff has no such bound; the caller falls back to
// an open upper end, which over-matches nothing because nothing sorts above it.
func prefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i, c := range slices.Backward(b) {
		if c < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}

// Cleanup removes events older than retentionDays.
func (s *SQLStore) Cleanup(retentionDays int) error {
	if s == nil || retentionDays <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	if _, err := s.db.ExecContext(ctx, deleteAuditBefore, db.TimeValue(cutoff)); err != nil {
		return fmt.Errorf("clean audit events: %w", err)
	}
	return nil
}

// Close is a no-op: the database handle outlives this store and belongs to whoever opened it.
func (s *SQLStore) Close() error { return nil }

// EventID derives a stable primary key from an event's own content.
//
// The trail has no id of its own, and the import has to be able to run the same day-file twice without duplicating it — which happens whenever a batch is in flight as the process dies. Two genuinely identical events at the same instant collapse into one row; that is the same thing the file trail's own de-duplication would have to do, and it is preferable to a re-import multiplying the history.
func EventID(e Event) string {
	doc, err := json.Marshal(e)
	if err != nil {
		doc = []byte(e.Type + e.TaskID + e.AgentID)
	}
	sum := sha256.Sum256(append([]byte(e.Timestamp.UTC().Format(time.RFC3339Nano)+"\x00"), doc...))
	return hex.EncodeToString(sum[:])
}

var _ Store = (*SQLStore)(nil)

// insertEventTx writes one event inside an import transaction.
func insertEventTx(ctx context.Context, database *db.DB, tx *sql.Tx, e Event, raw []byte) error {
	if _, err := tx.ExecContext(ctx, database.Rebind(insertAuditEvent),
		EventID(e), db.TimeValue(e.Timestamp), e.Type, e.TaskID, e.AgentID, string(raw)); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
