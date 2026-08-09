package attachment

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "attachments"

// Import copies attachment blobs and their metadata into the database once.
//
// The content moves, so the ordering has to come with it: each row keeps the
// created-at the metadata carried, which is what a listing sorts on. The files
// are only read, and an operator who dislikes the result still has them.
func Import(ctx context.Context, database *db.DB, root string, taskIDs []string, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	fileStore, err := NewStore(root, 0)
	if err != nil {
		return err
	}
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		written := 0
		for _, taskID := range taskIDs {
			metas, err := fileStore.List(taskID)
			if err != nil {
				// A task whose directory is unreadable is skipped rather than
				// stalling every other task's attachments behind it.
				logger.Warn("attachment.import.unreadable", "task_id", taskID, "err", err)
				continue
			}
			for i := range metas {
				data, meta, err := fileStore.Content(taskID, metas[i].ID)
				if err != nil {
					logger.Warn("attachment.import.blob_missing", "task_id", taskID,
						"attachment", metas[i].ID, "err", err)
					continue
				}
				if _, err := tx.ExecContext(ctx, database.Rebind(upsertAttachment),
					taskID, meta.ID, meta.FileName, meta.ContentType,
					int64(len(data)), db.TimeValue(meta.CreatedAt), data); err != nil {
					return 0, fmt.Errorf("insert attachment: %w", err)
				}
				written++
			}
		}
		return written, nil
	})
}
