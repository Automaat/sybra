// Package db opens and migrates Sybra's durable-storage backend.
//
// The query layer is deliberately plain database/sql with hand-written SQL and a
// per-dialect placeholder rewrite, not an ORM or a code generator: every store
// that moves off the filesystem has to run unchanged on both an embedded engine
// and a shared server one, and a thin dialect seam is the cheapest way to keep
// those two honest. Changing this choice after other stores depend on it is
// expensive — ask before doing it.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the postgres backend
	_ "modernc.org/sqlite"             // cgo-free database/sql driver "sqlite" for the embedded backend
)

// Dialect names the SQL flavor a DB speaks. Stores branch on it only where the two engines genuinely disagree.
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Options describes one backend connection. It mirrors config.DatabaseConfig without importing it, so internal/config stays free of a database dependency.
type Options struct {
	Backend         string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DB is an open, migrated backend handle shared by every SQL-backed store.
type DB struct {
	sqlDB   *sql.DB
	dialect Dialect
}

const (
	defaultSQLiteMaxOpenConns   = 1
	defaultPostgresMaxOpenConns = 16
)

// Open connects to the configured backend, verifies it is reachable, and applies any pending schema migrations. Every failure names the backend and the redacted DSN so an operator can tell a wrong setting from an unreachable server.
func Open(ctx context.Context, opts Options) (*DB, error) {
	dialect, driver, err := resolveDriver(opts.Backend)
	if err != nil {
		return nil, err
	}
	dsn, err := prepareDSN(dialect, opts.DSN)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s database %s: %w", dialect, RedactDSN(opts.DSN), err)
	}
	applyPool(sqlDB, dialect, opts)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("reach %s database %s: %w", dialect, RedactDSN(opts.DSN), err)
	}

	d := &DB{sqlDB: sqlDB, dialect: dialect}
	if err := Migrate(ctx, d); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate %s database %s: %w", dialect, RedactDSN(opts.DSN), err)
	}
	return d, nil
}

// New wraps an already-open *sql.DB. Tests use it to share one server across cases; production code goes through Open.
func New(sqlDB *sql.DB, dialect Dialect) *DB {
	return &DB{sqlDB: sqlDB, dialect: dialect}
}

// SQL exposes the underlying handle for stores that need transactions or driver-specific calls.
func (d *DB) SQL() *sql.DB { return d.sqlDB }

// Dialect reports which SQL flavor this handle speaks.
func (d *DB) Dialect() Dialect { return d.dialect }

// Close releases the connection pool.
func (d *DB) Close() error {
	if d == nil || d.sqlDB == nil {
		return nil
	}
	return d.sqlDB.Close()
}

// Rebind converts a query written with '?' placeholders into the dialect's own form. Stores write '?' everywhere and call this once per statement.
func (d *DB) Rebind(query string) string {
	if d == nil || d.dialect != Postgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := range len(query) {
		c := query[i]
		if c != '?' {
			b.WriteByte(c)
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// ExecContext runs a '?'-placeholder statement against the backend.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.sqlDB.ExecContext(ctx, d.Rebind(query), args...)
}

// QueryContext runs a '?'-placeholder query against the backend.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.sqlDB.QueryContext(ctx, d.Rebind(query), args...)
}

// QueryRowContext runs a '?'-placeholder single-row query against the backend.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.sqlDB.QueryRowContext(ctx, d.Rebind(query), args...)
}

// InTx runs fn inside a transaction, rolling back on error or panic. Stores use it for any write that must land whole.
func (d *DB) InTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

func resolveDriver(backend string) (Dialect, string, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case string(SQLite), "sqlite3":
		return SQLite, "sqlite", nil
	case string(Postgres), "postgresql", "pgx":
		return Postgres, "pgx", nil
	default:
		return "", "", fmt.Errorf("unsupported database backend %q (valid: %s, %s)", backend, SQLite, Postgres)
	}
}

// sqlitePragmas are carried in the DSN rather than executed after Open so
// every connection the pool ever creates gets them. foreign_keys and
// synchronous are per-connection settings, so a post-Open PRAGMA would apply
// to whichever connection happened to serve it and silently miss the rest of
// a pool sized above one.
var sqlitePragmas = []string{"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)"}

// prepareDSN normalizes a DSN and rejects the empty case per dialect.
func prepareDSN(dialect Dialect, dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("%s database needs a dsn (database.dsn is empty)", dialect)
	}
	if dialect != SQLite {
		return dsn, nil
	}
	return prepareSQLiteDSN(dsn)
}

// prepareSQLiteDSN turns a plain path into a file: URL and adds the default pragmas. An operator who spelled their own _pragma keeps full control of the set.
func prepareSQLiteDSN(dsn string) (string, error) {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + filepath.ToSlash(dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse sqlite dsn %s: %w", RedactDSN(dsn), err)
	}
	query := u.Query()
	if len(query["_pragma"]) == 0 {
		query["_pragma"] = sqlitePragmas
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func applyPool(sqlDB *sql.DB, dialect Dialect, opts Options) {
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultPostgresMaxOpenConns
		if dialect == SQLite {
			maxOpen = defaultSQLiteMaxOpenConns
		}
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	idle := opts.MaxIdleConns
	if idle <= 0 {
		idle = maxOpen
	}
	sqlDB.SetMaxIdleConns(idle)
	if opts.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
}

// RedactDSN strips any password from a DSN so it can appear in an error or a log line.
func RedactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "(empty)"
	}
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "redacted")
			return u.String()
		}
		return dsn
	}
	fields := strings.Fields(dsn)
	for i, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "password=") {
			fields[i] = "password=redacted"
		}
	}
	return strings.Join(fields, " ")
}
