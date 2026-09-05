package artifact

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "artifacts"

// Import copies artifact content and metadata into the database once.
//
// Each row keeps the name and created-at the file carried, which is what a
// listing orders on. The files are only read.
func Import(ctx context.Context, database *db.DB, root, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	fileStore := New(root)
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		taskIDs, err := fileStore.ListTaskIDs()
		if err != nil {
			return 0, fmt.Errorf("list artifact tasks: %w", err)
		}
		written := 0
		for _, taskID := range taskIDs {
			metas, err := fileStore.List(taskID)
			if err != nil {
				logger.Warn("artifact.import.unreadable", "task_id", taskID, "err", err)
				continue
			}
			for i := range metas {
				content, meta, err := fileStore.Read(taskID, metas[i].Name)
				if err != nil {
					logger.Warn("artifact.import.blob_missing", "task_id", taskID,
						"artifact", metas[i].Name, "err", err)
					continue
				}
				if _, err := tx.ExecContext(ctx, database.Rebind(upsertArtifact),
					taskID, meta.Name, string(meta.Kind), int64(len(content)),
					db.TimeValue(meta.CreatedAt), db.TimeValue(meta.CreatedAt), content); err != nil {
					return 0, fmt.Errorf("insert artifact: %w", err)
				}
				written++
			}
		}
		return written, nil
	})
}
