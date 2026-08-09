package experience

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "experience"

// Import copies the per-project record files into the database once.
//
// The files are only read. An operator who flips the backend and dislikes the
// result still has them, and an import interrupted halfway commits nothing, so
// the next start retries against an untouched directory.
func Import(ctx context.Context, database *db.DB, dir string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		return importRecords(ctx, database, tx, dir)
	})
}

func importRecords(ctx context.Context, database *db.DB, tx *sql.Tx, dir string) (int, error) {
	projects, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		// Nothing has ever written a record. A fresh install is a legitimate
		// zero, not a failure to import.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read experience dir: %w", err)
	}
	written := 0
	for _, entry := range projects {
		if !entry.IsDir() {
			continue
		}
		projectKey := entry.Name()
		files, err := os.ReadDir(filepath.Join(dir, projectKey))
		if err != nil {
			return 0, fmt.Errorf("read project experience dir: %w", err)
		}
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, projectKey, file.Name()))
			if err != nil {
				return 0, fmt.Errorf("read experience record: %w", err)
			}
			var rec Record
			if err := json.Unmarshal(data, &rec); err != nil {
				// The file store already skips what it cannot parse, so an
				// unreadable record is not new breakage and must not stop the
				// whole import — which would retry and fail again forever.
				continue
			}
			recordID, err := sanitizeRecordID(rec.TaskID)
			if err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertExperienceSQLite),
				projectKey, recordID, db.TimeValue(rec.CreatedAt), string(data)); err != nil {
				return 0, fmt.Errorf("insert experience record: %w", err)
			}
			written++
		}
	}
	return written, nil
}
