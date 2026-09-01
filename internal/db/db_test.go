package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestOpen_UnknownBackendNamesTheSetting(t *testing.T) {
	_, err := db.Open(t.Context(), db.Options{Backend: "mysql", DSN: "whatever"})
	if err == nil {
		t.Fatal("expected an error for an unsupported backend")
	}
	for _, want := range []string{"mysql", "sqlite", "postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestOpen_EmptyDSNNamesTheSetting(t *testing.T) {
	_, err := db.Open(t.Context(), db.Options{Backend: "postgres", DSN: "  "})
	if err == nil {
		t.Fatal("expected an error for an empty dsn")
	}
	if !strings.Contains(err.Error(), "database.dsn") {
		t.Errorf("error %q does not name database.dsn", err)
	}
}

func TestOpen_SQLiteWALFailureNamesBackendAndRedactedDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), "CREATE TABLE fixture (id INTEGER PRIMARY KEY)"); err != nil {
		_ = raw.Close()
		t.Fatalf("create fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	dsn := filepath.ToSlash(path) + "?mode=ro&password=hunter2&_pragma=busy_timeout(10)"
	d, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: dsn})
	if d != nil {
		_ = d.Close()
	}
	if err == nil {
		t.Fatal("open read-only rollback-journal database succeeded, want WAL configuration failure")
	}
	for _, want := range []string{"sqlite", filepath.Base(path), "password=redacted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaked password: %q", err)
	}
}

func TestOpen_UnreachableServerFails(t *testing.T) {
	// Port 1 on the loopback has nothing listening, so this exercises the
	// reachability probe rather than a malformed-DSN rejection.
	_, err := db.Open(t.Context(), db.Options{
		Backend: "postgres",
		DSN:     "postgres://sybra:secret@127.0.0.1:1/sybra?connect_timeout=2",
	})
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	if !strings.Contains(err.Error(), "reach postgres database") {
		t.Errorf("error %q does not say the server was unreachable", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error %q leaks the dsn password", err)
	}
}

func TestMigrate_CreatesSchemaAndIsIdempotent(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		first, err := db.SchemaVersion(t.Context(), d)
		if err != nil {
			t.Fatalf("schema version: %v", err)
		}
		if first == 0 {
			t.Fatal("expected Open to have applied at least one migration")
		}
		if err := db.Migrate(t.Context(), d); err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		second, err := db.SchemaVersion(t.Context(), d)
		if err != nil {
			t.Fatalf("schema version after second migrate: %v", err)
		}
		if second != first {
			t.Errorf("second migrate changed the schema version: %d -> %d", first, second)
		}
	})
}

func TestMigrate_RefusesADatabaseFromANewerBuild(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		_, err := d.ExecContext(t.Context(),
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			999999, "from_the_future", "deadbeef", 1)
		if err != nil {
			t.Fatalf("stamp a future migration: %v", err)
		}
		err = db.Migrate(t.Context(), d)
		if !errors.Is(err, db.ErrSchemaAhead) {
			t.Fatalf("expected ErrSchemaAhead, got %v", err)
		}
	})
}

func TestSQLiteData_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	first, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.ExecContext(t.Context(),
		`INSERT INTO loop_agents (id, name, prompt, interval_sec, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"abc123", "durable", "/noop", 60, 1, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	var name string
	if err := second.QueryRowContext(t.Context(), `SELECT name FROM loop_agents WHERE id = ?`, "abc123").Scan(&name); err != nil {
		t.Fatalf("read back after reopen: %v", err)
	}
	if name != "durable" {
		t.Errorf("name = %q, want %q", name, "durable")
	}
}

func TestRebind(t *testing.T) {
	tests := []struct {
		name    string
		dialect db.Dialect
		query   string
		want    string
	}{
		{"sqlite keeps question marks", db.SQLite, "SELECT a FROM t WHERE b = ? AND c = ?", "SELECT a FROM t WHERE b = ? AND c = ?"},
		{"postgres numbers placeholders", db.Postgres, "SELECT a FROM t WHERE b = ? AND c = ?", "SELECT a FROM t WHERE b = $1 AND c = $2"},
		{"postgres without placeholders", db.Postgres, "SELECT 1", "SELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := db.New(nil, tt.dialect).Rebind(tt.query)
			if got != tt.want {
				t.Errorf("Rebind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"url with password", "postgres://sybra:hunter2@db:5432/sybra", "postgres://sybra:redacted@db:5432/sybra"},
		{"sqlite file URL query password", "file:/data/sybra.db?mode=ro&password=hunter2", "file:/data/sybra.db?mode=ro&password=redacted"},
		{"sqlite plain path query password", "/data/sybra.db?mode=ro&password=hunter2", "/data/sybra.db?mode=ro&password=redacted"},
		{"url without password", "postgres://sybra@db:5432/sybra", "postgres://sybra@db:5432/sybra"},
		{"keyword form", "host=db user=sybra password=hunter2 dbname=sybra", "host=db user=sybra password=redacted dbname=sybra"},
		{"keyword password with question mark", "host=db user=sybra password='hunter?2' dbname=sybra", "host=db user=sybra password=redacted dbname=sybra"},
		{"empty", "  ", "(empty)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := db.RedactDSN(tt.dsn); got != tt.want {
				t.Errorf("RedactDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

func TestSQLitePragmasApplyToEveryPooledConnection(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{
		Backend:      "sqlite",
		DSN:          filepath.Join(t.TempDir(), "sybra.db"),
		MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// Hold four connections at once so each is a distinct pooled connection,
	// then read a per-connection pragma back from every one of them.
	conns := make([]*sql.Conn, 0, 4)
	for range 4 {
		conn, err := d.SQL().Conn(t.Context())
		if err != nil {
			t.Fatalf("check out connection: %v", err)
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		var foreignKeys int
		if err := conn.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("read foreign_keys on connection %d: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("connection %d has foreign_keys=%d, want 1", i, foreignKeys)
		}
		var cacheSize, mmapSize, tempStore int64
		var journalSizeLimit int64
		var autoCheckpointPages int64
		if err := conn.QueryRowContext(t.Context(), "PRAGMA cache_size").Scan(&cacheSize); err != nil {
			t.Fatalf("read cache_size on connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(t.Context(), "PRAGMA mmap_size").Scan(&mmapSize); err != nil {
			t.Fatalf("read mmap_size on connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(t.Context(), "PRAGMA temp_store").Scan(&tempStore); err != nil {
			t.Fatalf("read temp_store on connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(t.Context(), "PRAGMA journal_size_limit").Scan(&journalSizeLimit); err != nil {
			t.Fatalf("read journal_size_limit on connection %d: %v", i, err)
		}
		if err := conn.QueryRowContext(t.Context(), "PRAGMA wal_autocheckpoint").Scan(&autoCheckpointPages); err != nil {
			t.Fatalf("read wal_autocheckpoint on connection %d: %v", i, err)
		}
		if cacheSize != -16384 || mmapSize != 268435456 || tempStore != 2 {
			t.Errorf("connection %d tuning = cache_size %d, mmap_size %d, temp_store %d", i, cacheSize, mmapSize, tempStore)
		}
		if journalSizeLimit != 16<<20 {
			t.Errorf("connection %d journal_size_limit = %d, want %d", i, journalSizeLimit, 16<<20)
		}
		if autoCheckpointPages != 1000 {
			t.Errorf("connection %d wal_autocheckpoint = %d, want 1000", i, autoCheckpointPages)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close connection %d: %v", i, err)
		}
	}
}

func TestSQLiteDefaultPoolAllowsConcurrentReaders(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN:     filepath.Join(t.TempDir(), "sybra.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if got := d.SQL().Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("MaxOpenConnections = %d, want 4", got)
	}
}

func TestSQLiteDefaultPoolReadDoesNotQueueBehindWriter(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN:     filepath.Join(t.TempDir(), "sybra.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.ExecContext(t.Context(), `CREATE TABLE pool_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	writer, err := d.SQL().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(t.Context(), `INSERT INTO pool_probe (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	readCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var count int
	if err := d.QueryRowContext(readCtx, `SELECT COUNT(*) FROM pool_probe`).Scan(&count); err != nil {
		t.Fatalf("read queued behind active writer: %v", err)
	}
	if count != 0 {
		t.Fatalf("reader saw uncommitted row: count=%d", count)
	}
}

func TestSQLitePrivateMemoryPoolStaysOnOneConnection(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if got := d.SQL().Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 for a private in-memory database", got)
	}
	if _, err := d.ExecContext(t.Context(), `CREATE TABLE memory_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), `INSERT INTO memory_probe (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_probe`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestSQLiteSharedMemoryPoolKeepsConfiguredConcurrency(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{
		Backend:      "sqlite",
		DSN:          "file:shared-pool?mode=memory&cache=shared",
		MaxOpenConns: 3,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if got := d.SQL().Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("MaxOpenConnections = %d, want configured shared-memory pool of 3", got)
	}
}

func TestPrepareSQLiteDSN_KeepsAnOperatorsOwnPragmas(t *testing.T) {
	dir := t.TempDir()
	// An explicit _pragma set means the operator owns the whole set; the
	// defaults must not be merged in behind their back.
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN:     "file:" + filepath.Join(dir, "sybra.db") + "?_pragma=foreign_keys(0)",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var foreignKeys int
	if err := d.SQL().QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Errorf("foreign_keys = %d, want the operator's 0", foreignKeys)
	}
}

func TestPrepareSQLiteDSN_EnforcesWALPolicyOverOperatorOverrides(t *testing.T) {
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN: "file:" + filepath.Join(t.TempDir(), "sybra.db") +
			`?_pragma=main."journal_size_limit"(-1)&_pragma=main.[wal_autocheckpoint](0)`,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var journalSizeLimit int64
	if err := d.QueryRowContext(t.Context(), "PRAGMA journal_size_limit").Scan(&journalSizeLimit); err != nil {
		t.Fatalf("read journal_size_limit: %v", err)
	}
	if journalSizeLimit != 16<<20 {
		t.Errorf("journal_size_limit = %d, want product bound %d", journalSizeLimit, 16<<20)
	}
	var autoCheckpointPages int64
	if err := d.QueryRowContext(t.Context(), "PRAGMA wal_autocheckpoint").Scan(&autoCheckpointPages); err != nil {
		t.Fatalf("read wal_autocheckpoint: %v", err)
	}
	if autoCheckpointPages != 1000 {
		t.Errorf("wal_autocheckpoint = %d, want product threshold 1000", autoCheckpointPages)
	}
	var busyTimeout int64
	if err := d.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want retry fallback 5000", busyTimeout)
	}
	var synchronous int64
	if err := d.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("read synchronous: %v", err)
	}
	if synchronous != 2 {
		t.Errorf("synchronous = %d, want SQLite's implicit FULL (2)", synchronous)
	}
}

func TestSQLiteConcurrentOpensKeepDefaultsAfterRemovingOnlyOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	dsn := "file:" + filepath.ToSlash(path) +
		`?_pragma=main."journal_size_limit"(-1)&_pragma=main.[wal_autocheckpoint](0)`
	const starts = 8
	errs := make(chan error, starts)
	var wg sync.WaitGroup
	for range starts {
		wg.Go(func() {
			d, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: dsn})
			if d != nil {
				_ = d.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent open failed after override filtering: %v", err)
		}
	}
}

func TestSQLiteOpenShrinksExistingOversizedWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(t.Context(), `CREATE TABLE wal_probe (payload BLOB NOT NULL)`); err != nil {
		t.Fatalf("create fixture table: %v", err)
	}
	const payloadBytes = 32 << 20
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO wal_probe (payload) VALUES (zeroblob(?))`, payloadBytes); err != nil {
		t.Fatalf("create oversized WAL: %v", err)
	}
	if got := fileSize(t, path+"-wal"); got <= 16<<20 {
		t.Fatalf("fixture WAL is only %d bytes, want more than %d", got, 16<<20)
	}

	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN: "file:" + filepath.ToSlash(path) +
			`?_pragma=main.[journal_size_limit](-1)&_pragma=main."wal_autocheckpoint"(0)`,
	})
	if err != nil {
		t.Fatalf("open bounded database: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if got := fileSize(t, path+"-wal"); got > 16<<20 {
		t.Errorf("WAL after bounded open = %d bytes, want at most %d", got, 16<<20)
	}
	var mode string
	if err := d.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal mode = %q, want WAL", mode)
	}
	var storedBytes int
	if err := d.QueryRowContext(t.Context(), `SELECT length(payload) FROM wal_probe`).Scan(&storedBytes); err != nil {
		t.Fatalf("read committed payload: %v", err)
	}
	if storedBytes != payloadBytes {
		t.Errorf("stored payload = %d bytes, want %d", storedBytes, payloadBytes)
	}
}

func TestSQLiteOpenContinuesWhenStartupCheckpointHasActiveReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	raw.SetMaxOpenConns(2)
	raw.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(t.Context(), `CREATE TABLE reader_probe (payload BLOB NOT NULL)`); err != nil {
		t.Fatalf("create fixture table: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO reader_probe (payload) VALUES (zeroblob(1))`); err != nil {
		t.Fatalf("seed fixture table: %v", err)
	}
	reader, err := raw.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	defer reader.Rollback()
	var count int
	if err := reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reader_probe`).Scan(&count); err != nil {
		t.Fatalf("establish reader snapshot: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO reader_probe (payload) VALUES (zeroblob(1048576))`); err != nil {
		t.Fatalf("write after reader snapshot: %v", err)
	}

	// TRUNCATE cannot reset a WAL while this reader owns the older snapshot.
	// Open must still succeed; the per-connection bound will take effect after
	// the reader leaves and a later checkpoint resets the log.
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN:     "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(10)",
	})
	if err != nil {
		t.Fatalf("open while startup checkpoint is busy: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
}

func TestSQLiteWALBoundDoesNotRejectLargeTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sybra.db")
	d, err := db.Open(t.Context(), db.Options{
		Backend: "sqlite",
		DSN: "file:" + filepath.ToSlash(path) +
			`?_pragma=main.[journal_size_limit](-1)&_pragma=main."wal_autocheckpoint"(0)`,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.ExecContext(t.Context(), `CREATE TABLE large_write (payload BLOB NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	const payloadBytes = 32 << 20
	if _, err := d.ExecContext(t.Context(), `INSERT INTO large_write (payload) VALUES (zeroblob(?))`, payloadBytes); err != nil {
		t.Fatalf("write larger than WAL bound: %v", err)
	}
	// The large commit crosses the enforced automatic-checkpoint threshold and
	// resets the WAL. SQLite applies the retained-size limit on the first commit
	// in the next WAL generation; the operator's disabling override cannot stop
	// either step.
	if _, err := d.ExecContext(t.Context(), `INSERT INTO large_write (payload) VALUES (zeroblob(1))`); err != nil {
		t.Fatalf("commit after WAL reset: %v", err)
	}
	if got := fileSize(t, path+"-wal"); got > 16<<20 {
		t.Errorf("reset WAL = %d bytes, want at most %d", got, 16<<20)
	}
	var storedBytes int
	if err := d.QueryRowContext(t.Context(), `SELECT MAX(length(payload)) FROM large_write`).Scan(&storedBytes); err != nil {
		t.Fatalf("read committed payload: %v", err)
	}
	if storedBytes != payloadBytes {
		t.Errorf("stored payload = %d bytes, want %d", storedBytes, payloadBytes)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func TestMigrate_RefusesAnEditedMigration(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		// Editing a shipped migration instead of adding a new one reaches a
		// fresh database and silently misses every existing one, so CI stays
		// green while the operator's board lacks the column.
		if _, err := d.ExecContext(t.Context(),
			`UPDATE schema_migrations SET checksum = ? WHERE version = ?`, "tampered", 1); err != nil {
			t.Fatalf("rewrite stored checksum: %v", err)
		}
		err := db.Migrate(t.Context(), d)
		if !errors.Is(err, db.ErrMigrationChanged) {
			t.Fatalf("expected ErrMigrationChanged, got %v", err)
		}
	})
}

func TestMigrate_ConcurrentStartsAgainstOneDatabase(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, e dbtest.Engine) {
		t.Helper()
		// Several instances sharing one board restart together after an
		// auto-update. Without a migration lock they race on CREATE TABLE and
		// the version INSERT, and all but one abort startup.
		const starts = 6
		errs := make(chan error, starts)
		var wg sync.WaitGroup
		for range starts {
			wg.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						errs <- fmt.Errorf("panic: %v", r)
					}
				}()
				e.Open(t)
				errs <- nil
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent start failed: %v", err)
			}
		}
	})
}

func TestRedactDSN_DoesNotLeakAPasswordQueryParameter(t *testing.T) {
	// pgx accepts a password as a query parameter, so the userinfo check alone
	// leaves it in the error text and the app log.
	got := db.RedactDSN("postgres://sybra@db:5432/sybra?password=hunter2&sslmode=require")
	if strings.Contains(got, "hunter2") {
		t.Errorf("RedactDSN leaked the password: %q", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("RedactDSN dropped an unrelated parameter: %q", got)
	}
}

func TestRedactDSN_HandlesQuotedKeywordValues(t *testing.T) {
	got := db.RedactDSN("host=db user=sybra password='hunter 2' dbname=sybra")
	want := "host=db user=sybra password=redacted dbname=sybra"
	if got != want {
		t.Errorf("RedactDSN() = %q, want %q", got, want)
	}
}

func TestRebind_LeavesLiteralsAndEscapesAlone(t *testing.T) {
	tests := []struct {
		name    string
		dialect db.Dialect
		query   string
		want    string
	}{
		{
			name:    "question mark inside a literal is not a placeholder",
			dialect: db.Postgres,
			query:   "SELECT a FROM t WHERE name LIKE '%?%' AND b = ?",
			want:    "SELECT a FROM t WHERE name LIKE '%?%' AND b = $1",
		},
		{
			name:    "doubled question mark is an escaped operator",
			dialect: db.Postgres,
			query:   "SELECT a FROM t WHERE data ?? 'key' AND b = ?",
			want:    "SELECT a FROM t WHERE data ? 'key' AND b = $1",
		},
		{
			name:    "sqlite unescapes the operator too",
			dialect: db.SQLite,
			query:   "SELECT a FROM t WHERE data ?? 'key' AND b = ?",
			want:    "SELECT a FROM t WHERE data ? 'key' AND b = ?",
		},
		{
			name:    "doubled quote inside a literal does not end it",
			dialect: db.Postgres,
			query:   "SELECT a FROM t WHERE name = 'it''s ?' AND b = ?",
			want:    "SELECT a FROM t WHERE name = 'it''s ?' AND b = $1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := db.New(nil, tt.dialect).Rebind(tt.query); got != tt.want {
				t.Errorf("Rebind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMigrate_WaitsOutABusyTimeoutItLoses(t *testing.T) {
	// Given many instances starting together on a board whose write lock
	// expires long before the slowest of them gets a turn
	dsn := "file:" + filepath.Join(t.TempDir(), "sybra.db") + "?_pragma=busy_timeout(50)"
	const starts = 24
	errs := make(chan error, starts)
	var wg sync.WaitGroup
	for range starts {
		wg.Go(func() {
			handle, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: dsn})
			if handle != nil {
				t.Cleanup(func() { _ = handle.Close() })
			}
			errs <- err
		})
	}

	// When each of them migrates the one database
	wg.Wait()
	close(errs)

	// Then losing the lock costs a wait, not a start
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent start failed: %v", err)
		}
	}
}
