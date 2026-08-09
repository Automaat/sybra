package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// outboxQueryTimeout bounds every statement. The outbox is written from the
// retry loop and the sink, neither of which carries a context.
const outboxQueryTimeout = 15 * time.Second

// SQLIssueOutbox keeps pending issue filings in the configured database backend.
//
// One row per fingerprint, written whole, so a crash leaves the row at its
// previous value rather than a half-written file — which for this store means
// an entry that neither files nor retries.
type SQLIssueOutbox struct {
	db     *db.DB
	logger *slog.Logger
}

// NewSQLIssueOutbox returns the database-backed outbox.
func NewSQLIssueOutbox(database *db.DB, logger *slog.Logger) (*SQLIssueOutbox, error) {
	if database == nil {
		return nil, errors.New("issue outbox needs an open database")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SQLIssueOutbox{db: database, logger: logger}, nil
}

const (
	upsertOutboxItem = `INSERT INTO issue_outbox (fingerprint, operation, attempts, first_failed_at, doc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			operation = excluded.operation, attempts = excluded.attempts,
			first_failed_at = excluded.first_failed_at, doc = excluded.doc`

	deleteOutboxItem = `DELETE FROM issue_outbox WHERE fingerprint = ?`

	selectOutboxItems = `SELECT doc FROM issue_outbox ORDER BY first_failed_at, `
)

func (s *SQLIssueOutbox) put(it outboxItem) error {
	if it.Fingerprint == "" {
		return errors.New("monitor: outbox item has no fingerprint")
	}
	doc, err := yaml.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal outbox item: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), outboxQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, upsertOutboxItem,
		it.Fingerprint, it.Operation, int64(it.Attempts), db.TimeValue(it.FirstFailedAt), string(doc)); err != nil {
		return fmt.Errorf("write outbox item: %w", err)
	}
	return nil
}

func (s *SQLIssueOutbox) del(fingerprint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), outboxQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteOutboxItem, fingerprint); err != nil {
		return fmt.Errorf("delete outbox item: %w", err)
	}
	return nil
}

// load returns every pending filing, oldest failure first so the retry loop
// drains in the order entries were stranded.
func (s *SQLIssueOutbox) load(log *slog.Logger) []outboxItem {
	if log == nil {
		log = s.logger
	}
	ctx, cancel := context.WithTimeout(context.Background(), outboxQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectOutboxItems+s.db.OrderText("fingerprint"))
	if err != nil {
		log.Warn("monitor.issue_outbox.load.query-failed", "err", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []outboxItem
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			log.Warn("monitor.issue_outbox.load.scan-failed", "err", err)
			return out
		}
		var it outboxItem
		if err := yaml.Unmarshal([]byte(doc), &it); err != nil {
			log.Warn("monitor.issue_outbox.load.parse-failed", "err", err)
			continue
		}
		if it.Fingerprint == "" {
			continue
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		log.Warn("monitor.issue_outbox.load.iterate-failed", "err", err)
	}
	return out
}

// get returns one pending filing.
func (s *SQLIssueOutbox) get(fingerprint string) (outboxItem, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), outboxQueryTimeout)
	defer cancel()
	var doc string
	if err := s.db.QueryRowContext(ctx,
		`SELECT doc FROM issue_outbox WHERE fingerprint = ?`, fingerprint).Scan(&doc); err != nil {
		return outboxItem{}, false
	}
	var it outboxItem
	if err := yaml.Unmarshal([]byte(doc), &it); err != nil {
		return outboxItem{}, false
	}
	return it, true
}

// depth counts pending filings in the database rather than by loading them.
func (s *SQLIssueOutbox) depth() int {
	ctx, cancel := context.WithTimeout(context.Background(), outboxQueryTimeout)
	defer cancel()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_outbox`).Scan(&n); err != nil {
		s.logger.Warn("monitor.issue_outbox.depth-failed", "err", err)
		return 0
	}
	return n
}

var _ IssueOutboxPersistence = (*SQLIssueOutbox)(nil)
