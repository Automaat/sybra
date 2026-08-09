// Package dbimport moves a domain's existing files into the database exactly
// once, so flipping storage.database.backend does not silently start from an
// empty board.
package dbimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

// LockImport serializes the whole import across processes. Two instances
// sharing one postgres board both start, both find no marker, and both copy
// every file in without it.
const LockImport db.LockKey = 6_215_034_129_003

const (
	selectImport = `SELECT record_count FROM schema_imports WHERE domain = ?`
	insertImport = `INSERT INTO schema_imports (domain, imported_at, record_count) VALUES (?, ?, ?)`
)

// Once copies one instance's files for a domain into the database the first
// time it is called, and does nothing on every start after that.
//
// The marker is keyed by domain AND scope, where scope identifies the home the
// files came from. A postgres board is shared by several machines, each with
// its own home and its own files: a marker keyed by domain alone would let
// whichever instance started first claim the domain, and every other machine's
// records would sit unimported forever with nothing reporting it.
//
// fn reports how many records it wrote. The marker row is inserted in the same
// transaction as those writes, so an interrupted run commits neither and the
// next start retries from files that were never touched — fn must only read
// them. A domain whose files are already gone is a legitimate zero.
func Once(ctx context.Context, database *db.DB, domain, scope string, logger *slog.Logger, fn func(context.Context, *sql.Tx) (int, error)) error {
	if database == nil {
		return errors.New("dbimport: needs an open database")
	}
	if domain == "" {
		return errors.New("dbimport: needs a domain name")
	}
	if strings.TrimSpace(scope) == "" {
		return errors.New("dbimport: needs a scope naming whose files these are")
	}
	key := domain + "@" + scope
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return database.InTxLocked(ctx, LockImport, func(tx *sql.Tx) error {
		var count int64
		err := tx.QueryRowContext(ctx, database.Rebind(selectImport), key).Scan(&count)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("dbimport %s: read marker: %w", domain, err)
		}

		written, err := fn(ctx, tx)
		if err != nil {
			return fmt.Errorf("dbimport %s: %w", domain, err)
		}
		if _, err := tx.ExecContext(ctx, database.Rebind(insertImport),
			key, db.TimeValue(time.Now().UTC()), int64(written)); err != nil {
			return fmt.Errorf("dbimport %s: record marker: %w", domain, err)
		}
		logger.Info("db.import.done", "domain", domain, "scope", scope, "records", written)
		return nil
	})
}

// Imported reports whether a domain has already been imported. It exists for
// tests and for callers that need to order work around the import, not as a
// substitute for the marker Once writes.
func Imported(ctx context.Context, database *db.DB, domain, scope string) (bool, error) {
	if database == nil {
		return false, errors.New("dbimport: needs an open database")
	}
	var count int64
	err := database.QueryRowContext(ctx, selectImport, domain+"@"+scope).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dbimport %s: read marker: %w", domain, err)
	}
	return true, nil
}
