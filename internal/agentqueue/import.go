package agentqueue

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "agentqueue"

// Import copies the queued items into the database once.
//
// Bounded by the queue's own depth, so one transaction. The files are only
// read; an interrupted import commits neither the rows nor the marker.
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		fileStore, err := newStore(dir)
		if err != nil {
			return 0, err
		}
		items := fileStore.load(logger)
		for i := range items {
			it := items[i]
			doc, err := yaml.Marshal(it)
			if err != nil {
				return 0, fmt.Errorf("marshal item: %w", err)
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertQueueItem),
				it.TaskID, it.Role, string(it.Priority), string(it.Status), string(doc)); err != nil {
				return 0, fmt.Errorf("insert queue item: %w", err)
			}
		}
		return len(items), nil
	})
}
