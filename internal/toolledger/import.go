package toolledger

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "toolledger"

// Import copies the per-day ledger files into the database, one file per batch.
//
// Resumable rather than all-or-nothing: a tool-call ledger is an unbounded history, and a single transaction over years of it would be discarded whole by one interruption. The cursor is the last day-file consumed, committed with that file's rows, so a restart resumes at the next file and a file retried after a crash cannot duplicate — the row key is a digest of the record itself.
//
// The files are only read.
func Import(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	return dbimport.Resumable(ctx, database, ImportDomain, scope, logger,
		func(ctx context.Context, tx *sql.Tx, cursor string) (dbimport.Batch, error) {
			return importNextDay(ctx, database, tx, dir, cursor)
		})
}

func importNextDay(ctx context.Context, database *db.DB, tx *sql.Tx, dir, cursor string) (dbimport.Batch, error) {
	days, err := ledgerFiles(dir)
	if err != nil {
		return dbimport.Batch{}, err
	}
	next := ""
	for _, name := range days {
		if name > cursor {
			next = name
			break
		}
	}
	if next == "" {
		return dbimport.Batch{Cursor: cursor, Done: true}, nil
	}

	count, err := importLedgerFile(ctx, database, tx, filepath.Join(dir, next))
	if err != nil {
		return dbimport.Batch{}, err
	}
	return dbimport.Batch{Cursor: next, Count: count}, nil
}

func ledgerFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tool ledger dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ndjson" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func importLedgerFile(ctx context.Context, database *db.DB, tx *sql.Tx, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open tool ledger file: %w", err)
	}
	defer func() { _ = f.Close() }()

	written := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// A ledger truncated by a crash ends in exactly one unreadable
			// line, so failing here would stall the import on a file it could
			// never get past.
			continue
		}
		if err := insertRecordTx(ctx, database, tx, r, []byte(line)); err != nil {
			return 0, err
		}
		written++
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read tool ledger file: %w", err)
	}
	return written, nil
}
