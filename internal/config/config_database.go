package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Database backend identifiers accepted by database.backend. An unset key resolves to DBBackendFile, so an untouched config needs no migration.
const (
	DBBackendFile     = "file"
	DBBackendSQLite   = "sqlite"
	DBBackendPostgres = "postgres"
)

// DefaultSQLiteFileName is the database file created under the Sybra home when database.backend is sqlite and no dsn is given.
const DefaultSQLiteFileName = "sybra.db"

// DatabaseConfig selects the durable-storage backend and its connection settings. Omitting the block keeps every store on the filesystem.
type DatabaseConfig struct {
	// Backend is "file" (filesystem stores), "sqlite" (embedded single file), or "postgres" (shared server). An unrecognized value fails validation instead of falling back, so a typo cannot silently keep writing files.
	Backend string `yaml:"backend,omitempty" json:"backend"`
	// DSN is a file path for sqlite (default ~/.sybra/sybra.db) and a required postgres:// URL or key=value string for postgres.
	DSN string `yaml:"dsn,omitempty" json:"dsn" secret:"true"`
	// MaxOpenConns caps concurrent connections; 0 uses the per-engine default (1 for sqlite, 16 for postgres).
	MaxOpenConns int `yaml:"max_open_conns,omitempty" json:"maxOpenConns"`
	// MaxIdleConns caps pooled idle connections; 0 uses the per-engine default.
	MaxIdleConns int `yaml:"max_idle_conns,omitempty" json:"maxIdleConns"`
	// ConnMaxLifetimeSeconds retires a pooled connection after this age; 0 keeps it until the driver drops it.
	ConnMaxLifetimeSeconds int `yaml:"conn_max_lifetime_seconds,omitempty" json:"connMaxLifetimeSeconds"`
}

// NormalizeDBBackend canonicalizes a database.backend value. Casing and surrounding space are tolerated so a formatting slip never changes which engine the board lands on; unknown values are rejected.
func NormalizeDBBackend(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", DBBackendFile:
		return DBBackendFile, nil
	case DBBackendSQLite, "sqlite3":
		return DBBackendSQLite, nil
	case DBBackendPostgres, "postgresql", "pgx":
		return DBBackendPostgres, nil
	default:
		return "", fmt.Errorf("invalid database.backend %q (valid: %s, %s, %s)",
			s, DBBackendFile, DBBackendSQLite, DBBackendPostgres)
	}
}

// DatabaseBackend returns the resolved backend identifier. An invalid value resolves to file here because ValidateResolvedConfig already refuses to start on it, so this accessor never has to guess.
func (c *Config) DatabaseBackend() string {
	if c == nil {
		return DBBackendFile
	}
	backend, err := NormalizeDBBackend(c.Database.Backend)
	if err != nil {
		return DBBackendFile
	}
	return backend
}

// DatabaseEnabled reports whether durable state goes to a database rather than the filesystem stores.
func (c *Config) DatabaseEnabled() bool {
	return c.DatabaseBackend() != DBBackendFile
}

// DatabaseDSN returns the connection string for the resolved backend, filling in the default sqlite path when the operator named the engine but no location. Postgres has no sensible default; validation rejects an empty one.
func (c *Config) DatabaseDSN() string {
	if c == nil {
		return ""
	}
	dsn := strings.TrimSpace(c.Database.DSN)
	if dsn == "" && c.DatabaseBackend() == DBBackendSQLite {
		return filepath.Join(HomeDir(), DefaultSQLiteFileName)
	}
	return dsn
}

func validateDatabaseConfig(cfg *ResolvedConfig, add func(string, ...any)) {
	backend, err := NormalizeDBBackend(cfg.Database.Backend)
	if err != nil {
		add("%s", err.Error())
		return
	}
	if backend == DBBackendFile {
		return
	}
	if backend == DBBackendPostgres && strings.TrimSpace(cfg.Database.DSN) == "" {
		add("database.dsn is required when database.backend is %s", DBBackendPostgres)
	}
	if cfg.Database.MaxOpenConns < 0 {
		add("database.max_open_conns must be >= 0 (got %d)", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.MaxIdleConns < 0 {
		add("database.max_idle_conns must be >= 0 (got %d)", cfg.Database.MaxIdleConns)
	}
	if cfg.Database.ConnMaxLifetimeSeconds < 0 {
		add("database.conn_max_lifetime_seconds must be >= 0 (got %d)", cfg.Database.ConnMaxLifetimeSeconds)
	}
}
