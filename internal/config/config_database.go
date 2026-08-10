package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Database backend identifiers accepted by database.backend. An unset key resolves to DBBackendSQLite; the file stores are being retired and an untouched config migrates itself on first start.
const (
	DBBackendFile     = "file"
	DBBackendSQLite   = "sqlite"
	DBBackendPostgres = "postgres"
)

// DefaultSQLiteFileName is the database file created under the Sybra home when database.backend is sqlite and no dsn is given.
const DefaultSQLiteFileName = "sybra.db"

// DatabaseConfig selects the durable-storage backend and its connection settings. Omitting the block lands on sqlite under the Sybra home; the filesystem stores are reached only by naming "file" explicitly, and are being retired.
type DatabaseConfig struct {
	// Backend is "sqlite" (embedded single file, the default when unset), "postgres" (shared server), or "file" (the filesystem stores, retained for rollback and being retired). An unrecognized value fails validation instead of falling back, so a typo cannot silently change where the board lives.
	Backend string `yaml:"backend,omitempty" json:"backend"`
	// DSN is a file path for sqlite (default ~/.sybra/sybra.db), optionally with driver query parameters after a "?", and a required postgres:// URL or key=value string for postgres.
	DSN string `yaml:"dsn,omitempty" json:"dsn" secret:"true"`
	// MaxOpenConns caps concurrent connections; 0 uses the per-engine default (4 for sqlite, 16 for postgres).
	MaxOpenConns int `yaml:"max_open_conns,omitempty" json:"maxOpenConns"`
	// MaxIdleConns caps pooled idle connections; 0 uses the per-engine default.
	MaxIdleConns int `yaml:"max_idle_conns,omitempty" json:"maxIdleConns"`
	// ConnMaxLifetimeSeconds retires a pooled connection after this age; 0 keeps it until the driver drops it.
	ConnMaxLifetimeSeconds int `yaml:"conn_max_lifetime_seconds,omitempty" json:"connMaxLifetimeSeconds"`
}

// NormalizeDBBackend canonicalizes a database.backend value. Casing and surrounding space are tolerated so a formatting slip never changes which engine the board lands on; unknown values are rejected.
func NormalizeDBBackend(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		// Unset lands on sqlite rather than file: the file stores are being retired, and an install that never touched this key still migrates itself on first start through the per-domain imports.
		return DBBackendSQLite, nil
	case DBBackendFile:
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

// DatabaseBackend returns the resolved backend identifier.
//
// A nil receiver or an invalid value resolves to file, which is the inert answer rather than the default one: ValidateResolvedConfig already refuses to start on a bad value, and a nil config describes no install to migrate. An unset key on a real config resolves to sqlite through NormalizeDBBackend.
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
