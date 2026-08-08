package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// ErrMigrationChanged reports a migration file edited after it was applied somewhere. The edit reaches a fresh database and silently misses every existing one, so it must be a new migration instead.
var ErrMigrationChanged = errors.New("applied migration no longer matches the shipped file")

// migrationLockKey is the postgres advisory-lock key held for the whole of
// Migrate. Several instances share one board by design, and they restart
// together after an auto-update, so without it concurrent CREATE TABLE and
// version INSERT collide and abort startup with a catalog error that names no
// setting the operator can act on.
const migrationLockKey int64 = 6_215_034_129_001

type migration struct {
	version  int
	name     string
	body     string
	checksum string
}

// Migrate applies every pending migration for the handle's dialect. It is idempotent, so a second start is a no-op, and it fails rather than downgrade a database stamped with a version this build does not know.
func Migrate(ctx context.Context, d *DB) error {
	return d.withMigrationLock(ctx, func() error {
		return migrateLocked(ctx, d)
	})
}

// migrateLocked runs the whole sequence in one transaction so the version
// read and the version write cannot interleave with another process's. On
// sqlite that transaction is the only cross-process serialization available
// (BEGIN IMMEDIATE, via the DSN's _txlock); on postgres it backs up the
// advisory lock.
func migrateLocked(ctx context.Context, d *DB) error {
	available, err := loadMigrations(d.dialect)
	if err != nil {
		return err
	}
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, createVersionTable); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		applied, err := appliedMigrations(ctx, d, tx)
		if err != nil {
			return err
		}
		if err := verifyApplied(applied, available); err != nil {
			return err
		}
		for _, m := range available {
			if _, done := applied[m.version]; done {
				continue
			}
			if err := applyMigration(ctx, d, tx, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// withMigrationLock serializes migration across processes on postgres. SQLite has a single writer already, so it runs fn directly.
func (d *DB) withMigrationLock(ctx context.Context, fn func() error) error {
	if d.dialect != Postgres {
		return fn()
	}
	conn, err := d.sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()
	return fn()
}

// SchemaVersion returns the highest applied migration version, or 0 for a database with no migrations yet.
func SchemaVersion(ctx context.Context, d *DB) (int, error) {
	var applied map[int]string
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, createVersionTable); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		var err error
		applied, err = appliedMigrations(ctx, d, tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	if len(applied) == 0 {
		return 0, nil
	}
	return slices.Max(slices.Collect(maps.Keys(applied))), nil
}

const createVersionTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version BIGINT PRIMARY KEY,
	name TEXT NOT NULL,
	checksum TEXT NOT NULL,
	applied_at BIGINT NOT NULL
)`

func appliedMigrations(ctx context.Context, d *DB, tx *sql.Tx) (map[int]string, error) {
	rows, err := tx.QueryContext(ctx, d.Rebind(`SELECT version, checksum FROM schema_migrations`))
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]string{}
	for rows.Next() {
		var (
			version  int
			checksum string
		)
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return out, nil
}

// verifyApplied rejects both directions of drift: a version this build has never heard of, and a version whose file changed since it was applied.
func verifyApplied(applied map[int]string, available []migration) error {
	known := make(map[int]migration, len(available))
	for _, m := range available {
		known[m.version] = m
	}
	for _, version := range slices.Sorted(maps.Keys(applied)) {
		m, ok := known[version]
		if !ok {
			return fmt.Errorf("%w: applied version %d is unknown to this build (highest known %d)",
				ErrSchemaAhead, version, highestVersion(available))
		}
		if stored := applied[version]; stored != "" && stored != m.checksum {
			return fmt.Errorf("%w: %04d_%s.sql changed after it was applied; ship the change as a new migration",
				ErrMigrationChanged, m.version, m.name)
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

func applyMigration(ctx context.Context, d *DB, tx *sql.Tx, m migration) error {
	for _, stmt := range splitStatements(m.body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	insert := d.Rebind(`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`)
	if _, err := tx.ExecContext(ctx, insert, m.version, m.name, m.checksum, time.Now().UTC().UnixMicro()); err != nil {
		return fmt.Errorf("record migration %04d_%s: %w", m.version, m.name, err)
	}
	return nil
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
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     name,
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
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
