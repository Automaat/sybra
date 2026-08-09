package stats

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// ImportDomain names this domain in the import marker table.
const ImportDomain = "stats"

// importBatchSize bounds one transaction. Run history is unbounded, and a single transaction over all of it would hold locks for as long as the copy takes and be discarded whole by one interruption.
const importBatchSize = 2000

// Import copies the run history file into the database in batches.
//
// The cursor is the number of lines already consumed, committed with the rows from that batch, so a crash resumes at the next unread line. A batch retried after a crash cannot duplicate: the row key is the run's own id.
//
// The file is only read.
func Import(ctx context.Context, database *db.DB, path, scope string, logger *slog.Logger) error {
	return dbimport.Resumable(ctx, database, ImportDomain, scope, logger,
		func(ctx context.Context, tx *sql.Tx, cursor string) (dbimport.Batch, error) {
			return importBatch(ctx, database, tx, path, cursor)
		})
}

func importBatch(ctx context.Context, database *db.DB, tx *sql.Tx, path, cursor string) (dbimport.Batch, error) {
	skip, err := parseCursor(cursor)
	if err != nil {
		return dbimport.Batch{}, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return dbimport.Batch{Cursor: cursor, Done: true}, nil
	}
	if err != nil {
		return dbimport.Batch{}, fmt.Errorf("open run history: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	read := 0
	written := 0
	for scanner.Scan() {
		read++
		if read <= skip {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			var r RunRecord
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				// The file store already tolerates a torn final line, which is
				// what a crash mid-append leaves. Failing here would stall the
				// import on a line it could never get past.
				r = RunRecord{}
			} else if err := insertRunTx(ctx, database, tx, r, []byte(line)); err != nil {
				return dbimport.Batch{}, err
			} else {
				written++
			}
		}
		if read-skip >= importBatchSize {
			return dbimport.Batch{Cursor: strconv.Itoa(read), Count: written}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return dbimport.Batch{}, fmt.Errorf("read run history: %w", err)
	}
	return dbimport.Batch{Cursor: strconv.Itoa(read), Count: written, Done: true}, nil
}

func parseCursor(cursor string) (int, error) {
	if strings.TrimSpace(cursor) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("run history import cursor %q is not a line count", cursor)
	}
	return n, nil
}
