package dispatch

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "attemptleases"

// Import copies the ledger document into the database once.
//
// Expiry and heartbeat are carried across verbatim rather than restamped: a
// lease is in flight precisely when its expiry is still ahead, so resetting it
// would either release every running agent's work at once or hold a dead
// agent's claim past its timeout. The issue's "in-flight leases survive the
// upgrade" is exactly this.
//
// Bounded, so one transaction: the ledger holds live attempts, not history.
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		state, err := newFilePersistence(filepath.Join(dir, "attempt-leases.yaml")).Load(ctx)
		if err != nil {
			return 0, err
		}
		for i := range state.Leases {
			rec := &state.Leases[i]
			doc, err := yaml.Marshal(rec)
			if err != nil {
				return 0, fmt.Errorf("encode attempt lease: %w", err)
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertLease),
				rec.ID, rec.Intent.TaskID, rec.Intent.Provider, string(rec.Status),
				db.TimeValue(rec.ExpiresAt), string(doc)); err != nil {
				return 0, fmt.Errorf("insert attempt lease: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(upsertRevision), ledgerName, revisionValue(state.Revision)); err != nil {
			return 0, fmt.Errorf("write ledger revision: %w", err)
		}
		return len(state.Leases), nil
	})
}
