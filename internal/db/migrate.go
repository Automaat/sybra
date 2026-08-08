package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations
var migrationFS embed.FS

// StatementSeparator splits one migration file into statements. It is explicit rather than a ';' split because pgx's extended protocol rejects multi-statement Exec and a naive split would break on a ';' inside a literal.
const StatementSeparator = "\n--;;\n"

// ErrSchemaAhead reports a database migrated by a newer build than this one. Running the older build's migrations against it would corrupt state, so Open refuses to start instead.
var ErrSchemaAhead = errors.New("database schema is newer than this build")

type migration struct {
	version int
	name    string
	body    string
}

// Migrate applies every pending migration for the handle's dialect. It is idempotent, so a second start is a no-op, and it fails rather than downgrade a database stamped with a version this build does not know.
func Migrate(ctx context.Context, d *DB) error {
	if err := ensureVersionTable(ctx, d); err != nil {
		return err
	}
	available, err := loadMigrations(d.dialect)
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, d)
	if err != nil {
		return err
	}
	if err := rejectUnknownVersions(applied, available); err != nil {
		return err
	}
	for _, m := range available {
		if slices.Contains(applied, m.version) {
			continue
		}
		if err := applyMigration(ctx, d, m); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version, or 0 for a database with no migrations yet.
func SchemaVersion(ctx context.Context, d *DB) (int, error) {
	applied, err := appliedVersions(ctx, d)
	if err != nil {
		return 0, err
	}
	if len(applied) == 0 {
		return 0, nil
	}
	return slices.Max(applied), nil
}

const createVersionTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version BIGINT PRIMARY KEY,
	name TEXT NOT NULL,
	applied_at BIGINT NOT NULL
)`

func ensureVersionTable(ctx context.Context, d *DB) error {
	if _, err := d.sqlDB.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, d *DB) ([]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return out, nil
}

func rejectUnknownVersions(applied []int, available []migration) error {
	known := make(map[int]struct{}, len(available))
	for _, m := range available {
		known[m.version] = struct{}{}
	}
	for _, v := range applied {
		if _, ok := known[v]; !ok {
			return fmt.Errorf("%w: applied version %d is unknown to this build (highest known %d)",
				ErrSchemaAhead, v, highestVersion(available))
		}
	}
	return nil
}

func highestVersion(available []migration) int {
	if len(available) == 0 {
		return 0
	}
	return available[len(available)-1].version
}

func applyMigration(ctx context.Context, d *DB, m migration) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range splitStatements(m.body) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
			}
		}
		insert := d.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`)
		if _, err := tx.ExecContext(ctx, insert, m.version, m.name, time.Now().UTC().UnixMicro()); err != nil {
			return fmt.Errorf("record migration %04d_%s: %w", m.version, m.name, err)
		}
		return nil
	})
}

func splitStatements(body string) []string {
	parts := strings.Split(body, StatementSeparator)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func loadMigrations(dialect Dialect) ([]migration, error) {
	dir := path.Join("migrations", string(dialect))
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations for %s: %w", dialect, err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationFS, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: name, body: string(body)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

func parseMigrationName(fileName string) (version int, name string, err error) {
	base := strings.TrimSuffix(fileName, ".sql")
	prefix, name, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %s must be named <version>_<name>.sql", fileName)
	}
	version, err = strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("migration %s has a non-numeric version prefix", fileName)
	}
	return version, name, nil
}
