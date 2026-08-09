package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement. Repository carries no context to cancel one with; see Repository.
const queryTimeout = 30 * time.Second

// SQLStore keeps run history in the configured database backend.
//
// A read for one task is a WHERE against an index rather than a scan of the whole history, and a record lands in one statement — so a crash leaves the row written or absent and the next start needs no tail repair, which the line-per-record file did.
type SQLStore struct {
	db     *db.DB
	logger *slog.Logger
}

// NewSQLStore returns the database-backed run history.
func NewSQLStore(database *db.DB, logger *slog.Logger) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("stats store needs an open database")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SQLStore{db: database, logger: logger}, nil
}

const insertRunRecord = `INSERT INTO run_records (id, task_id, project_id, started_at, doc)
	VALUES (?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`

// Record appends one run.
func (s *SQLStore) Record(r RunRecord) error {
	if s == nil {
		return nil
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal run record: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, insertRunRecord,
		r.ID, r.TaskID, r.ProjectID, db.TimeValue(r.Timestamp), string(doc)); err != nil {
		return fmt.Errorf("write run record: %w", err)
	}
	return nil
}

// Len reports how many runs are recorded, counted in the database rather than by loading them.
func (s *SQLStore) Len() int {
	if s == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_records`).Scan(&n); err != nil {
		s.logger.Error("stats.count", "err", err)
		return 0
	}
	return n
}

// All returns every run, oldest first.
func (s *SQLStore) All() []RunRecord {
	return s.load(`SELECT doc FROM run_records ORDER BY started_at, `+s.db.OrderText("id"), nil)
}

// AllForTask returns the runs attributed to taskID, reading only those rows.
func (s *SQLStore) AllForTask(taskID string) []RunRecord {
	return s.load(`SELECT doc FROM run_records WHERE task_id = ? ORDER BY started_at, `+s.db.OrderText("id"),
		[]any{taskID})
}

// Since returns the runs at or after t, reading only those rows. It is what a windowed report should use instead of All.
func (s *SQLStore) Since(t time.Time) []RunRecord {
	return s.load(`SELECT doc FROM run_records WHERE started_at >= ? ORDER BY started_at, `+s.db.OrderText("id"),
		[]any{db.TimeValue(t)})
}

// Query builds the stats response as of now.
func (s *SQLStore) Query() StatsResponse { return s.QueryAt(time.Now()) }

// QueryAt builds the stats response as of now.
//
// It loads every run because the response carries an all-time bucket; the windowed buckets are cut from the same slice by the shared Aggregate, so the figures are the file store's by construction rather than by a second implementation that agrees today.
func (s *SQLStore) QueryAt(now time.Time) StatsResponse {
	return Aggregate(s.All(), now)
}

func (s *SQLStore) load(stmt string, args []any) []RunRecord {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		s.logger.Error("stats.read", "err", err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []RunRecord
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			s.logger.Error("stats.scan", "err", err)
			return out
		}
		var r RunRecord
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("stats.iterate", "err", err)
	}
	return out
}

func insertRunTx(ctx context.Context, database *db.DB, tx *sql.Tx, r RunRecord, raw []byte) error {
	if _, err := tx.ExecContext(ctx, database.Rebind(insertRunRecord),
		r.ID, r.TaskID, r.ProjectID, db.TimeValue(r.Timestamp), string(raw)); err != nil {
		return fmt.Errorf("insert run record: %w", err)
	}
	return nil
}
