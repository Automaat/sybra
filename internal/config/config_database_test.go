package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDBBackend(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty defaults to sqlite", in: "", want: DBBackendSQLite},
		{name: "explicit file", in: "file", want: DBBackendFile},
		{name: "sqlite", in: "sqlite", want: DBBackendSQLite},
		{name: "sqlite3 alias", in: "sqlite3", want: DBBackendSQLite},
		{name: "postgres", in: "postgres", want: DBBackendPostgres},
		{name: "postgresql alias", in: "PostgreSQL", want: DBBackendPostgres},
		{name: "padded and mixed case", in: "  SQLite  ", want: DBBackendSQLite},
		{name: "unknown", in: "mysql", wantErr: `invalid database.backend "mysql"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeDBBackend(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NormalizeDBBackend(%q) error = %v, want one containing %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDBBackend(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeDBBackend(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestDatabaseEnabled_UnsetLandsOnSQLite pins the retirement of the file stores: an install that never named a backend still gets a database, and migrates itself there through the per-domain imports on first start.
func TestDatabaseEnabled_UnsetLandsOnSQLite(t *testing.T) {
	var cfg Config
	if !cfg.DatabaseEnabled() {
		t.Error("an omitted database block must now land on a database, not the filesystem stores")
	}
	if cfg.DatabaseBackend() != DBBackendSQLite {
		t.Errorf("DatabaseBackend() = %q, want %q", cfg.DatabaseBackend(), DBBackendSQLite)
	}
}

// TestDatabaseEnabled_FileStaysSelectable keeps the escape hatch honest while the file stores are still present.
func TestDatabaseEnabled_FileStaysSelectable(t *testing.T) {
	cfg := Config{Database: DatabaseConfig{Backend: DBBackendFile}}
	if cfg.DatabaseEnabled() {
		t.Error("an explicit file backend must still keep the filesystem stores")
	}
}

func TestDatabaseDSN_SQLiteDefaultsUnderTheSybraHome(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	cfg := Config{Database: DatabaseConfig{Backend: DBBackendSQLite}}
	want := filepath.Join(HomeDir(), DefaultSQLiteFileName)
	if got := cfg.DatabaseDSN(); got != want {
		t.Errorf("DatabaseDSN() = %q, want %q", got, want)
	}
}

func TestDatabaseDSN_PostgresHasNoDefault(t *testing.T) {
	cfg := Config{Database: DatabaseConfig{Backend: DBBackendPostgres}}
	if got := cfg.DatabaseDSN(); got != "" {
		t.Errorf("DatabaseDSN() = %q, want empty so validation can reject it", got)
	}
}

func TestValidateResolvedConfig_Database(t *testing.T) {
	tests := []struct {
		name    string
		db      DatabaseConfig
		wantErr string
	}{
		{name: "unset block", db: DatabaseConfig{}},
		{name: "sqlite without dsn", db: DatabaseConfig{Backend: DBBackendSQLite}},
		{
			name: "postgres with dsn",
			db:   DatabaseConfig{Backend: DBBackendPostgres, DSN: "postgres://localhost/sybra"},
		},
		{
			name:    "unknown backend",
			db:      DatabaseConfig{Backend: "mysql"},
			wantErr: "invalid database.backend",
		},
		{
			name:    "postgres without dsn",
			db:      DatabaseConfig{Backend: DBBackendPostgres},
			wantErr: "database.dsn is required",
		},
		{
			name:    "negative pool size",
			db:      DatabaseConfig{Backend: DBBackendSQLite, MaxOpenConns: -1},
			wantErr: "database.max_open_conns must be >= 0",
		},
		{
			name:    "negative idle size",
			db:      DatabaseConfig{Backend: DBBackendSQLite, MaxIdleConns: -2},
			wantErr: "database.max_idle_conns must be >= 0",
		},
		{
			name:    "negative lifetime",
			db:      DatabaseConfig{Backend: DBBackendSQLite, ConnMaxLifetimeSeconds: -5},
			wantErr: "database.conn_max_lifetime_seconds must be >= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Database = tt.db
			err := ValidateResolvedConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a validation error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validation error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
