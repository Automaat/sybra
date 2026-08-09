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
func Import(ctx context.Context, database *db.DB, path string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		ops, err := NewFilePersistence(path).Load()
		if err != nil {
			// A document this build cannot parse is not worth failing every start over: these records are progress reporting for operations that have already finished or died with the process that ran them.
			return 0, nil
		}
		if err := insertOps(ctx, database, tx, ops); err != nil {
			return 0, err
		}
		return len(ops), nil
	})
}
