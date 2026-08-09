package loopagent

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "loopagent"

// Import copies the per-file loop agent definitions into the database once.
//
// This domain moved to the database before the shared import existed, so an operator who switched backends silently started with no loop agents at all and their schedules simply stopped running. Existing installs pick them up on the first start after this lands.
func Import(ctx context.Context, database *db.DB, dir string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		store, err := NewStore(dir)
		if err != nil {
			return 0, err
		}
		agents, err := store.List(ctx)
		if err != nil {
			return 0, err
		}
		for i := range agents {
			if err := insertTx(ctx, database, tx, agents[i]); err != nil {
				return 0, err
			}
		}
		return len(agents), nil
	})
}

// insertTx writes one record exactly as it stands.
//
// Not Create: that mints a fresh id and fresh timestamps, which is right for a new agent and wrong for an import — the scheduler's run history and the GUI's links both key on the id the file already carried.
func insertTx(ctx context.Context, database *db.DB, tx *sql.Tx, la LoopAgent) error {
	tools, err := marshalTools(la.AllowedTools)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, database.Rebind(insertLoopAgent), insertArgs(la, tools)...); err != nil {
		return fmt.Errorf("insert loop agent: %w", err)
	}
	return nil
}
