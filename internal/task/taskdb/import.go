package taskdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
	"github.com/Automaat/sybra/internal/task"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "tasks"

// Import copies task files and their sidecars into the database once.
//
// A file that fails to parse is reported and skipped, and the import still
// completes: this is the board, and one unreadable task must not leave every
// other one unimported. Identifiers, timestamps and run history come across in
// the stored document, which is the same encoding the file carried.
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Once(ctx, database, ImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read tasks dir: %w", err)
		}

		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)

		written, skipped := 0, 0
		for _, name := range names {
			path := filepath.Join(dir, name)
			t, err := task.Parse(path)
			if err != nil {
				skipped++
				logger.Warn("task.import.unparseable", "path", path, "err", err,
					"reason", "this task is left on disk and not imported; every other task still imports")
				continue
			}
			doc, err := task.MarshalStored(t)
			if err != nil {
				skipped++
				logger.Warn("task.import.unencodable", "task_id", t.ID, "err", err)
				continue
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertTask),
				t.ID, string(t.Status), t.ProjectID, t.Title,
				db.TimeValue(t.CreatedAt), db.TimeValue(t.UpdatedAt), int64(0), string(doc)); err != nil {
				return 0, fmt.Errorf("insert task: %w", err)
			}
			written++

			for _, sc := range sidecarsOnDisk(dir, t.ID, logger) {
				if _, err := tx.ExecContext(ctx, database.Rebind(upsertSidecar),
					t.ID, sc.Kind, sc.Name, db.TimeValue(time.Now().UTC()), sc.Content); err != nil {
					return 0, fmt.Errorf("insert sidecar: %w", err)
				}
			}
		}
		if skipped > 0 {
			logger.Warn("task.import.skipped", "count", skipped,
				"reason", "these task files could not be read and remain on disk")
		}
		return written, nil
	})
}

// sidecarsOnDisk collects a task's companion documents by their filename
// suffix, which is how the file store recognized them.
func sidecarsOnDisk(dir, taskID string, logger *slog.Logger) []Sidecar {
	suffixes := map[string]string{
		".plan.md":                  SidecarPlan,
		".plan-contract.md":         SidecarPlanContract,
		".plan-critique.md":         SidecarPlanCritique,
		".plan-research.md":         SidecarPlanResearch,
		".plan-decisions.md":        SidecarPlanDecision,
		".plan-brief.md":            SidecarPlanBrief,
		".code-review.md":           SidecarCodeReview,
		".current-test-failures.md": SidecarCurrentTestFailures,
		".acceptance-ledger.md":     SidecarAcceptanceLedger,
		".spec-decisions.md":        SidecarSpecDecision,
		".comments.json":            SidecarComments,
	}
	var out []Sidecar
	for suffix, kind := range suffixes {
		path := filepath.Join(dir, taskID+suffix)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, Sidecar{Kind: kind, Content: string(data)})
	}

	// Drafts are several documents under one kind, separated by the name the
	// filename carries.
	prefix := taskID + ".plan-draft."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			logger.Warn("task.import.draft_unreadable", "file", entry.Name(), "err", err)
			continue
		}
		out = append(out, Sidecar{
			Kind:    SidecarPlanDraft,
			Name:    strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".md"),
			Content: string(data),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}
