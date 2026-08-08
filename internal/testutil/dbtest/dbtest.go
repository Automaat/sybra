// Package dbtest runs a test body against every database engine Sybra
// supports.
//
// A store that only ever sees sqlite drifts into sqlite-only SQL without
// anyone noticing, and the drift surfaces on the operator's postgres box
// rather than in CI. Each and Engines exist so a store's behavior suite is
// written once and executed on both.
//
// The postgres leg needs a reachable server named by SYBRA_TEST_POSTGRES_DSN.
// Setting SYBRA_REQUIRE_POSTGRES_TESTS=1 turns an absent or unreachable
// server into a failure — CI sets it, so a broken service container reports
// red instead of quietly dropping half the coverage.
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/db"
)

const (
	// PostgresDSNEnv names the environment variable holding a reachable postgres server for the postgres leg.
	PostgresDSNEnv = "SYBRA_TEST_POSTGRES_DSN"
	// RequirePostgresEnv makes a missing or unreachable postgres server a test failure rather than a skip.
	RequirePostgresEnv = "SYBRA_REQUIRE_POSTGRES_TESTS"
)

// Engine is one backend a test body runs against. Open returns a fresh handle
// on the same underlying database every call, so a test can close one handle
// and reopen to prove data survived a restart.
type Engine struct {
	Name    string
	Backend string
	Open    func(t *testing.T) *db.DB
}

// Each runs fn as a subtest per available engine.
func Each(t *testing.T, fn func(t *testing.T, e Engine)) {
	t.Helper()
	t.Run(string(db.SQLite), func(t *testing.T) {
		t.Helper()
		fn(t, sqliteEngine(t))
	})
	t.Run(string(db.Postgres), func(t *testing.T) {
		t.Helper()
		e, ok := postgresEngine(t)
		if !ok {
			return
		}
		fn(t, e)
	})
}

// Engines runs fn as a subtest per available engine, handing it one freshly migrated database.
func Engines(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	t.Helper()
	Each(t, func(t *testing.T, e Engine) {
		t.Helper()
		fn(t, e.Open(t))
	})
}

// SQLite opens a migrated embedded database in the test's temp dir.
func SQLite(t *testing.T) *db.DB {
	t.Helper()
	return sqliteEngine(t).Open(t)
}

// Postgres opens a migrated database inside a throwaway schema on the server named by SYBRA_TEST_POSTGRES_DSN, or skips when none is configured and CI has not demanded one.
func Postgres(t *testing.T) *db.DB {
	t.Helper()
	e, ok := postgresEngine(t)
	if !ok {
		return nil
	}
	return e.Open(t)
}

func sqliteEngine(t *testing.T) Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sybra.db")
	return Engine{
		Name:    string(db.SQLite),
		Backend: string(db.SQLite),
		Open: func(t *testing.T) *db.DB {
			t.Helper()
			return open(t, db.Options{Backend: string(db.SQLite), DSN: path})
		},
	}
}

func postgresEngine(t *testing.T) (Engine, bool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(PostgresDSNEnv))
	required := strings.TrimSpace(os.Getenv(RequirePostgresEnv)) != ""
	if dsn == "" {
		if required {
			t.Fatalf("%s is set but %s is unset", RequirePostgresEnv, PostgresDSNEnv)
		}
		t.Skipf("set %s to run the postgres engine tests", PostgresDSNEnv)
		return Engine{}, false
	}

	schema := "sybra_test_" + randomSuffix(t)
	if err := execOnServer(t.Context(), dsn, "CREATE SCHEMA "+schema); err != nil {
		if !required {
			t.Skipf("postgres at %s is not usable: %v", db.RedactDSN(dsn), err)
			return Engine{}, false
		}
		t.Fatalf("create schema %s on %s: %v", schema, db.RedactDSN(dsn), err)
	}
	t.Cleanup(func() {
		_ = execOnServer(context.Background(), dsn, "DROP SCHEMA "+schema+" CASCADE")
	})

	scoped, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("scope dsn to schema %s: %v", schema, err)
	}
	return Engine{
		Name:    string(db.Postgres),
		Backend: string(db.Postgres),
		Open: func(t *testing.T) *db.DB {
			t.Helper()
			return open(t, db.Options{Backend: string(db.Postgres), DSN: scoped})
		},
	}, true
}

func execOnServer(ctx context.Context, dsn, stmt string) error {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open %s: %w", db.RedactDSN(dsn), err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return err
	}
	return nil
}

func open(t *testing.T, opts db.Options) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("open %s database: %v", opts.Backend, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// withSearchPath pins every connection from the pool to one schema, so concurrent test binaries against a shared server never see each other's tables.
func withSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse dsn: %w", err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " search_path=" + schema, nil
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(buf)
}
