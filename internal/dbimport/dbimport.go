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

const (
	selectResume = `SELECT cursor, done FROM schema_imports WHERE domain = ?`
	upsertResume = `INSERT INTO schema_imports (domain, imported_at, record_count, cursor, done)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (domain) DO UPDATE SET
			imported_at = excluded.imported_at,
			record_count = schema_imports.record_count + excluded.record_count,
			cursor = excluded.cursor,
			done = excluded.done`
)

// Batch is one unit of a resumable import.
//
// Cursor is where the next batch resumes from, written in the same transaction as the rows so it can never claim progress that rolled back. Count accumulates into the operator-visible tally. Done marks the source exhausted; until it is set the next start resumes the domain rather than skipping it.
type Batch struct {
	Cursor string
	Count  int
	Done   bool
}

// Resumable copies a domain's logs into the database a batch at a time, continuing where the last run stopped.
//
// Once is wrong for this shape. These logs are append-only histories that grow without bound, so a single transaction can be arbitrarily large, and a crash halfway would discard hours of copying and start again from nothing. Here each batch commits its rows and its cursor together: a crash loses at most the batch in flight, and the next start picks up from the last committed cursor.
//
// next must tolerate being called twice for one cursor — the batch in flight when a process dies is retried — which the row primary keys provide.
func Resumable(ctx context.Context, database *db.DB, domain, scope string, logger *slog.Logger, next func(ctx context.Context, tx *sql.Tx, cursor string) (Batch, error)) error {
	if database == nil {
		return errors.New("dbimport: needs an open database")
	}
	if domain == "" {
		return errors.New("dbimport: needs a domain name")
	}
	if strings.TrimSpace(scope) == "" {
		return errors.New("dbimport: needs a scope naming whose files these are")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	key := domain + "@" + scope

	for {
		var (
			batch  Batch
			cursor string
			halt   bool
		)
		err := database.InTxLocked(ctx, LockImport, func(tx *sql.Tx) error {
			var done int64
			readErr := tx.QueryRowContext(ctx, database.Rebind(selectResume), key).Scan(&cursor, &done)
			switch {
			case readErr == nil && done != 0:
				halt = true
				return nil
			case readErr != nil && !errors.Is(readErr, sql.ErrNoRows):
				return fmt.Errorf("dbimport %s: read cursor: %w", domain, readErr)
			}

			var fnErr error
			batch, fnErr = next(ctx, tx, cursor)
			if fnErr != nil {
				return fmt.Errorf("dbimport %s: %w", domain, fnErr)
			}
			doneValue := int64(0)
			if batch.Done {
				doneValue = 1
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertResume),
				key, db.TimeValue(time.Now().UTC()), int64(batch.Count), batch.Cursor, doneValue); err != nil {
				return fmt.Errorf("dbimport %s: record cursor: %w", domain, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if halt {
			return nil
		}
		if batch.Done {
			logger.Info("db.import.done", "domain", domain, "scope", scope)
			return nil
		}
		if batch.Cursor == cursor {
			// A batch that neither finishes nor advances would spin forever
			// holding the import lock, so no other domain could import and
			// nothing would say why. That is a defect in the domain's own
			// next, and it is better reported than waited on.
			return fmt.Errorf("dbimport %s: batch did not advance past cursor %q", domain, cursor)
		}
		logger.Info("db.import.batch", "domain", domain, "scope", scope, "records", batch.Count, "cursor", batch.Cursor)
	}
}
