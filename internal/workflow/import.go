package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "workflow"

// Import copies workflow definitions and their snapshots into the database once.
//
// It must run before builtins are seeded. Seeding writes each builtin under its own id, so an import that ran afterwards would find those rows already present and overwrite a definition the operator had edited with the shipped one.
//
// The files are only read; an interrupted import commits neither the rows nor the marker, so the next start retries against an untouched directory.
func Import(ctx context.Context, database *db.DB, dir string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, ImportDomain, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		written, err := importDefinitions(ctx, database, tx, dir)
		if err != nil {
			return 0, err
		}
		snapshots, err := importSnapshots(ctx, database, tx, filepath.Join(dir, "snapshots"))
		if err != nil {
			return 0, err
		}
		return written + snapshots, nil
	})
}

func importDefinitions(ctx context.Context, database *db.DB, tx *sql.Tx, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read workflows dir: %w", err)
	}
	written := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return 0, fmt.Errorf("read workflow: %w", err)
		}
		def, err := parseDefinition(data)
		if err != nil {
			// The file store already skips what it cannot parse. Failing here would leave the import to retry and fail again on every start.
			continue
		}
		stamp := def.UpdatedAt
		if stamp.IsZero() {
			stamp = time.Now().UTC()
		}
		created := def.CreatedAt
		if created.IsZero() {
			created = stamp
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(upsertWorkflow),
			def.ID, def.Name, db.BoolValue(def.Builtin),
			db.TimeValue(created), db.TimeValue(stamp), string(data)); err != nil {
			return 0, fmt.Errorf("insert workflow: %w", err)
		}
		written++
	}
	return written, nil
}

func importSnapshots(ctx context.Context, database *db.DB, tx *sql.Tx, dir string) (int, error) {
	workflows, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read workflow snapshots dir: %w", err)
	}
	written := 0
	for _, wf := range workflows {
		if !wf.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, wf.Name()))
		if err != nil {
			return 0, fmt.Errorf("read workflow snapshot dir: %w", err)
		}
		for _, file := range files {
			hash := strings.TrimSuffix(file.Name(), ".yaml")
			if file.IsDir() || filepath.Ext(file.Name()) != ".yaml" || !snapshotHashPattern.MatchString(hash) {
				continue
			}
			path := filepath.Join(dir, wf.Name(), file.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return 0, fmt.Errorf("read workflow snapshot: %w", err)
			}
			stamp := time.Now().UTC()
			if info, statErr := os.Stat(path); statErr == nil {
				// The file's own mtime, so a snapshot keeps roughly the age it had rather than every one of them dating from the upgrade.
				stamp = info.ModTime().UTC()
			}
			var def Definition
			if err := yaml.Unmarshal(data, &def); err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(insertSnapshot),
				wf.Name(), hash, db.TimeValue(stamp), string(data)); err != nil {
				return 0, fmt.Errorf("insert workflow snapshot: %w", err)
			}
			written++
		}
	}
	return written, nil
}
