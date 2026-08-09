package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
)

// queryTimeout bounds every statement. The queue mirrors from paths that carry
// no context — an offer, a pop — so a stalled backend would otherwise hold one
// open with nothing able to cancel it.
const queryTimeout = 15 * time.Second

// SQLStore mirrors the queue into the configured database backend.
//
// One row per task, written whole, so a crash leaves the row at its previous
// value rather than a half-written file. Items are stored as the YAML the file
// store already used, so both agree by construction.
type SQLStore struct {
	db     *db.DB
	logger *slog.Logger
}

// NewSQLStore returns the database-backed queue mirror.
func NewSQLStore(database *db.DB, logger *slog.Logger) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("agent queue store needs an open database")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SQLStore{db: database, logger: logger}, nil
}

const (
	upsertQueueItem = `INSERT INTO agent_queue_items (task_id, role, priority, status, doc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
			role = excluded.role, priority = excluded.priority,
			status = excluded.status, doc = excluded.doc`

	deleteQueueItem = `DELETE FROM agent_queue_items WHERE task_id = ?`

	selectQueueItems = `SELECT doc FROM agent_queue_items ORDER BY `
)

func (s *SQLStore) put(it Item) error {
	if it.TaskID == "" {
		return errors.New("agentqueue: item has no task id")
	}
	doc, err := yaml.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal item: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, upsertQueueItem,
		it.TaskID, it.Role, string(it.Priority), string(it.Status), string(doc)); err != nil {
		return fmt.Errorf("write queue item: %w", err)
	}
	return nil
}

func (s *SQLStore) del(taskID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteQueueItem, taskID); err != nil {
		return fmt.Errorf("delete queue item: %w", err)
	}
	return nil
}

// load returns every mirrored item.
//
// Ordered by task id so a read is reproducible, which the file store's map
// iteration never was. Dispatch order does not come from here either way — the
// queue sorts by the persisted fields — so what matters is that every item
// comes back, not the sequence.
func (s *SQLStore) load(log *slog.Logger) []Item {
	if log == nil {
		log = s.logger
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectQueueItems+s.db.OrderText("task_id"))
	if err != nil {
		log.Warn("agentqueue.store.load.query-failed", "err", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []Item
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			log.Warn("agentqueue.store.load.scan-failed", "err", err)
			return out
		}
		var it Item
		if err := yaml.Unmarshal([]byte(doc), &it); err != nil {
			// Skipped and logged, as the file store does with an unparseable
			// file: one bad row must not cost the queue every other item.
			log.Warn("agentqueue.store.load.parse-failed", "err", err)
			continue
		}
		if it.TaskID == "" {
			log.Warn("agentqueue.store.load.empty-task-id")
			continue
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		log.Warn("agentqueue.store.load.iterate-failed", "err", err)
	}
	return out
}

var _ Persistence = (*SQLStore)(nil)
