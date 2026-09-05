package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// OutboxImportDomain names this domain in the import marker table.
const OutboxImportDomain = "issueoutbox"

// ImportIssueOutbox copies pending issue filings into the database once.
//
// Bounded — the sink caps the outbox — so one transaction. The files are only
// read. Losing an entry here files an issue twice or never, so an interrupted
// import committing nothing is the right failure.
func ImportIssueOutbox(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, OutboxImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		fileStore, err := newIssueOutboxStore(dir)
		if err != nil {
			return 0, err
		}
		items := fileStore.load(logger)
		for i := range items {
			it := items[i]
			doc, err := yaml.Marshal(it)
			if err != nil {
				return 0, fmt.Errorf("marshal outbox item: %w", err)
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertOutboxItem),
				it.Fingerprint, it.Operation, int64(it.Attempts),
				db.TimeValue(it.FirstFailedAt), string(doc)); err != nil {
				return 0, fmt.Errorf("insert outbox item: %w", err)
			}
		}
		return len(items), nil
	})
}
