package bgop

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "bgop"

// Import copies the persisted background operations into the database once.
//
// This domain is one document rather than a directory, so the import reads that one file. It is only read; an interrupted import commits neither the rows nor the marker and the next start retries from it.
//
// The imported rows belong to owner: the document was this instance's, and on a shared board another instance must not adopt or delete them.
func Import(ctx context.Context, database *db.DB, path, owner string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, ImportDomain, owner, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		// A document this build cannot parse imports as nothing rather than failing every start: these records are progress reporting for operations that already finished, or died with the process that ran them.
		ops, loadErr := NewFilePersistence(path).Load()
		if loadErr != nil {
			logger.Warn("bgop.import.unreadable", "path", path, "err", loadErr)
			ops = nil
		}
		if err := insertOps(ctx, database, tx, owner, ops); err != nil {
			return 0, err
		}
		return len(ops), nil
	})
}
