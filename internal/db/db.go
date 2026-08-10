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
	// WAL lets readers proceed while a writer is active. Keeping SQLite at one
	// connection defeats that benefit and makes every board refresh queue behind
	// workflow/audit writes, so keep a small pool while _txlock=immediate still
	// serializes write transactions safely.
	defaultSQLiteMaxOpenConns   = 4
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
	if err := d.ensureSQLiteWAL(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
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

// Rebind converts a query written with '?' placeholders into the dialect's own
// form. Stores write '?' everywhere and call this once per statement.
//
// A '?' inside a single- or double-quoted run is left alone, and '??' is an
// escape for one literal '?'. Both matter on postgres, where '?' is also the
// jsonb key-existence operator and a naive rewrite would turn it into a bind
// parameter the caller never supplied.
func (d *DB) Rebind(query string) string {
	if d == nil {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	placeholders := 0
	quote := byte(0)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == quote {
				if i+1 < len(query) && query[i+1] == quote {
					b.WriteByte(quote)
					i++
					continue
				}
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			b.WriteByte(c)
		case c == '?' && i+1 < len(query) && query[i+1] == '?':
			b.WriteByte('?')
			i++
		case c == '?':
			placeholders++
			d.writePlaceholder(&b, placeholders)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (d *DB) writePlaceholder(b *strings.Builder, n int) {
	if d.dialect != Postgres {
		b.WriteByte('?')
		return
	}
	b.WriteByte('$')
	b.WriteString(strconv.Itoa(n))
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

// LockKey names a cross-process advisory lock. Keys are constants so two
// callers cannot collide by accident.
type LockKey int64

// Advisory locks in use. Keep new keys distinct from these.
const (
	LockMigrations LockKey = 6_215_034_129_001
	LockSeedByName LockKey = 6_215_034_129_002
)

// InTxLocked runs fn in a transaction holding a cross-process advisory lock.
//
// It is what makes a check-then-insert atomic. A plain transaction is not
// enough: under READ COMMITTED, two concurrent "insert if absent" bodies both
// see no row and both insert. SQLite needs no extra lock — its immediate
// transaction already excludes every other writer.
func (d *DB) InTxLocked(ctx context.Context, key LockKey, fn func(*sql.Tx) error) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if d.dialect == Postgres {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(key)); err != nil {
				return fmt.Errorf("acquire advisory lock %d: %w", key, err)
			}
		}
		return fn(tx)
	})
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
var sqlitePragmas = []string{
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"synchronous(NORMAL)",
	// The default cache is only ~8 MiB and mmap is disabled. Sybra's task rows
	// contain sizeable durable workflow/run documents, so repeated board reads
	// otherwise churn through filesystem pages. These are per-connection caps;
	// mmap pages are shared by the process rather than copied into every pool
	// connection.
	"cache_size(-16384)",
	"mmap_size(268435456)",
	"temp_store(MEMORY)",
}

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

// prepareSQLiteDSN turns a plain path into a file: URL, adds the default
// pragmas, and opens transactions as writers.
//
// _txlock=immediate is what makes a read-modify-write inside InTx safe: a
// deferred transaction takes its read lock first and then fails the upgrade
// with SQLITE_BUSY_SNAPSHOT, which busy_timeout does not retry. An operator
// who spelled their own _pragma or _txlock keeps full control of that set.
func prepareSQLiteDSN(dsn string) (string, error) {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + filepath.ToSlash(dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse sqlite dsn %s: %w", RedactDSN(dsn), err)
	}
	query := u.Query()
	changed := false
	if len(query["_pragma"]) == 0 {
		query["_pragma"] = sqlitePragmas
		changed = true
	}
	if !query.Has("_txlock") {
		query.Set("_txlock", "immediate")
		changed = true
	}
	if changed {
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

// walSetupBudget bounds the retry on enabling the write-ahead log. Two
// processes opening a brand-new file race, and a journal-mode change is one of
// the few statements SQLite will not run under the busy handler, so it has to
// be retried by hand.
const walSetupBudget = 5 * time.Second

// ensureSQLiteWAL switches the file to the write-ahead log once.
//
// The mode is a property of the file, not of a connection, so it is read first
// and only written when it differs — that keeps every connection after the
// first out of the contended path entirely. It is deliberately not one of the
// DSN pragmas: applied per-connection it fails with SQLITE_BUSY whenever
// another connection holds the file, which turns a concurrent open into a
// startup abort.
func (d *DB) ensureSQLiteWAL(ctx context.Context) error {
	if d.dialect != SQLite {
		return nil
	}
	deadline := time.Now().Add(walSetupBudget)
	var lastErr error
	for {
		var mode string
		if err := d.sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			lastErr = err
		} else if strings.EqualFold(mode, "wal") {
			return nil
		} else if _, err := d.sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("enable sqlite write-ahead log: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func applyPool(sqlDB *sql.DB, dialect Dialect, opts Options) {
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultPostgresMaxOpenConns
		if dialect == SQLite {
			maxOpen = defaultSQLiteMaxOpenConns
		}
	}
	// SQLite's private in-memory databases exist per connection. A pool larger
	// than one would intermittently see an empty schema whenever database/sql
	// selected a different connection. Callers that explicitly choose
	// cache=shared keep the configured pool because their DSN opts into one
	// shared in-memory database.
	if dialect == SQLite && sqlitePrivateMemoryDSN(opts.DSN) {
		maxOpen = 1
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

func sqlitePrivateMemoryDSN(dsn string) bool {
	dsn = strings.TrimSpace(dsn)
	if dsn == ":memory:" {
		return true
	}
	if !strings.HasPrefix(dsn, "file:") {
		return false
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	query := u.Query()
	isMemory := u.Path == ":memory:" || strings.EqualFold(query.Get("mode"), "memory")
	return isMemory && !strings.EqualFold(query.Get("cache"), "shared")
}

// RedactDSN strips any password from a DSN so it can appear in an error or a log line.
func RedactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "(empty)"
	}
	if strings.Contains(dsn, "://") {
		return redactURLDSN(dsn)
	}
	return redactKeywordDSN(dsn)
}

const redactedValue = "redacted"

// redactURLDSN covers both places a URL DSN can carry a secret: the userinfo
// segment and a password query parameter, which pgx honours just as readily.
func redactURLDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return redactKeywordDSN(dsn)
	}
	changed := false
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), redactedValue)
			changed = true
		}
	}
	if query := u.Query(); query.Has("password") {
		query.Set("password", redactedValue)
		u.RawQuery = query.Encode()
		changed = true
	}
	if !changed {
		return dsn
	}
	return u.String()
}

// redactKeywordDSN walks libpq keyword/value pairs. It cannot split on
// whitespace: a quoted value may contain spaces, and splitting inside one
// leaves the tail of the password in the output.
func redactKeywordDSN(dsn string) string {
	var b strings.Builder
	b.Grow(len(dsn))
	i := 0
	for i < len(dsn) {
		start := i
		for i < len(dsn) && dsn[i] == ' ' {
			i++
		}
		b.WriteString(dsn[start:i])
		if i >= len(dsn) {
			break
		}
		key, next := scanKeywordKey(dsn, i)
		if next >= len(dsn) || dsn[next] != '=' {
			b.WriteString(dsn[i:next])
			i = next
			continue
		}
		value, after := scanKeywordValue(dsn, next+1)
		b.WriteString(key)
		b.WriteByte('=')
		if strings.EqualFold(key, "password") {
			b.WriteString(redactedValue)
		} else {
			b.WriteString(value)
		}
		i = after
	}
	return b.String()
}

func scanKeywordKey(dsn string, i int) (key string, next int) {
	start := i
	for i < len(dsn) && dsn[i] != '=' && dsn[i] != ' ' {
		i++
	}
	return dsn[start:i], i
}

func scanKeywordValue(dsn string, i int) (value string, next int) {
	start := i
	if i < len(dsn) && dsn[i] == '\'' {
		i++
		for i < len(dsn) {
			if dsn[i] == '\\' && i+1 < len(dsn) {
				i += 2
				continue
			}
			if dsn[i] == '\'' {
				i++
				break
			}
			i++
		}
		return dsn[start:i], i
	}
	for i < len(dsn) && dsn[i] != ' ' {
		i++
	}
	return dsn[start:i], i
}

// OrderText renders an ORDER BY term that sorts by byte value on every engine.
//
// Postgres orders text by the server's collation, and the deploy target's
// en_US.UTF-8 ignores punctuation at the primary level: "pr-review" sorts after
// "prompt-lab-author" there and before it on sqlite. The file stores these
// tables replace listed a directory, which is byte order, and the workflow
// engine breaks priority ties on exactly that order — so without this, which
// workflow a task dispatches depends on the engine and the server's locale.
func (d *DB) OrderText(column string) string {
	if d.dialect == Postgres {
		return column + ` COLLATE "C"`
	}
	return column
}
