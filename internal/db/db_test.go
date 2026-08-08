package db_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			999999, "from_the_future", 1)
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
		{"url without password", "postgres://sybra@db:5432/sybra", "postgres://sybra@db:5432/sybra"},
		{"keyword form", "host=db user=sybra password=hunter2 dbname=sybra", "host=db user=sybra password=redacted dbname=sybra"},
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
		if err := conn.Close(); err != nil {
			t.Errorf("close connection %d: %v", i, err)
		}
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
