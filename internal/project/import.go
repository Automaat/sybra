package project

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
	"github.com/Automaat/sybra/internal/fsutil"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "projects"

// Import copies the project files into the database once.
//
// Bounded by however many repositories an operator registered, so one
// transaction. The files are only read, and the clones are not touched at all:
// each imported record keeps the clone path it already had, which is how the
// clones stay matched to their records.
//
// A record whose clone is missing is imported and reported, not dropped. It is
// the operator's project either way, and losing the record would leave the
// clone on disk with nothing pointing at it.
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		paths, err := fsutil.ListFiles(dir, ".yaml")
		if os.IsNotExist(err) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read projects dir: %w", err)
		}
		written := 0
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return 0, fmt.Errorf("read project: %w", err)
			}
			var p Project
			if err := yaml.Unmarshal(data, &p); err != nil {
				// Skipping matches the store, which already ignores a file it
				// cannot parse. Failing would stall the import on it for ever.
				logger.Warn("project.import.unreadable", "path", path, "err", err)
				continue
			}
			if p.ID == "" {
				continue
			}
			if !CloneUsable(p) {
				logger.Warn("project.import.clone_missing", "project", p.ID, "clone_path", p.ClonePath,
					"reason", "the record is imported, but nothing will work against this project until its clone is restored")
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertProject),
				p.ID, p.Owner, p.Repo, string(p.Type), string(p.Status), p.ClonePath,
				db.TimeValue(p.UpdatedAt), string(data)); err != nil {
				return 0, fmt.Errorf("insert project: %w", err)
			}
			written++
		}
		return written, nil
	})
}
