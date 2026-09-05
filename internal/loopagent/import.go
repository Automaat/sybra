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
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		store, err := NewStore(dir)
		if err != nil {
			return 0, err
		}
		agents, err := store.List(ctx)
		if err != nil {
			return 0, err
		}
		taken, err := namesInUse(ctx, database, tx)
		if err != nil {
			return 0, err
		}
		written := 0
		for i := range agents {
			// Reconciled by name, not inserted blind. #3235 swapped the store
			// while the table was empty, so the first-boot seed re-created
			// sybra-self-monitor under a fresh id while the operator's file
			// stayed on disk. A blind import gives that schedule a second row
			// and the scheduler then runs it twice, at double cost.
			if _, exists := taken[agents[i].Name]; exists {
				continue
			}
			if err := insertTx(ctx, database, tx, agents[i]); err != nil {
				return 0, err
			}
			taken[agents[i].Name] = struct{}{}
			written++
		}
		return written, nil
	})
}

// namesInUse reads the names already in the table, inside the import's own transaction so a concurrent create cannot slip between the read and the inserts.
func namesInUse(ctx context.Context, database *db.DB, tx *sql.Tx) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, database.Rebind(`SELECT name FROM loop_agents`))
	if err != nil {
		return nil, fmt.Errorf("read loop agent names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	taken := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan loop agent name: %w", err)
		}
		taken[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop agent names: %w", err)
	}
	return taken, nil
}

// insertTx writes one record exactly as it stands.
//
// Not Create: that mints a fresh id and fresh timestamps, which is right for a new agent and wrong for an import — the scheduler's run history and the GUI's links both key on the id the file already carried.
func insertTx(ctx context.Context, database *db.DB, tx *sql.Tx, la LoopAgent) error {
	tools, err := marshalTools(la.AllowedTools)
	if err != nil {
		return err
	}
	// DO NOTHING on a duplicate id: two files can carry one id (the store lists
	// by directory, not by filename), and a failed insert would abort the whole
	// import — which, because the marker commits with the rows, then fails
	// identically on every start and never imports anything at all.
	if _, err := tx.ExecContext(ctx, database.Rebind(insertLoopAgent+` ON CONFLICT (id) DO NOTHING`), insertArgs(la, tools)...); err != nil {
		return fmt.Errorf("insert loop agent: %w", err)
	}
	return nil
}
