package cleanup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ProtectedImportDomain names this domain in the import marker table.
const ProtectedImportDomain = "protectedfindings"

// ImportProtected copies the protected-findings document into the database
// once.
//
// Bounded — a finding resolves once its cause is cleared — so one transaction.
// The document is only read; losing a finding here is what lets a later
// cleanup pass delete the path it protects, so an interrupted import
// committing nothing is the right failure.
func ImportProtected(ctx context.Context, database *db.DB, path, scope string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ProtectedImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		rec, err := (&protectedFiles{path: path}).Read()
		if err != nil {
			return 0, err
		}
		for i := range rec.Findings {
			f := &rec.Findings[i]
			doc, err := json.Marshal(f)
			if err != nil {
				return 0, fmt.Errorf("protected findings: encode: %w", err)
			}
			if _, err := tx.ExecContext(ctx, upsertProtectedStmt(database),
				f.ID, string(f.Kind), db.TimeValue(f.FirstSeenAt), string(doc)); err != nil {
				return 0, fmt.Errorf("insert protected finding: %w", err)
			}
		}
		return len(rec.Findings), nil
	})
}
