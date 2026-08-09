package limits

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "limits"

// Import copies the quota document into the database once.
//
// One document rather than a growing log, and bounded by the event retention the file store already applied, so this one fits in a transaction and needs no cursor.
//
// The file is only read.
func Import(ctx context.Context, database *db.DB, path, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		var p persisted
		if len(data) > 0 {
			if err := json.Unmarshal(data, &p); err != nil {
				// A document this build cannot parse imports as nothing rather
				// than failing every start: quota state is re-observed from the
				// providers within one poll interval.
				logger.Warn("limits.import.unreadable", "path", path, "err", err)
				return 0, nil
			}
		}
		for provider := range p.Snapshots {
			s := p.Snapshots[provider]
			doc, err := json.Marshal(&s)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertQuotaSnapshot),
				s.Provider, db.TimeValue(s.CapturedAt), string(doc)); err != nil {
				return 0, err
			}
		}
		if err := insertEventsTx(ctx, database, tx, p.Events); err != nil {
			return 0, err
		}
		return len(p.Snapshots) + len(p.Events), nil
	})
}
