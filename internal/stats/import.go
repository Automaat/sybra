package stats

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"unicode"

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
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return dbimport.Resumable(ctx, database, ImportDomain, scope, logger,
		func(ctx context.Context, tx *sql.Tx, cursor string) (dbimport.Batch, error) {
			batch, err := importBatch(ctx, database, tx, path, cursor)
			if err == nil && batch.Done {
				warnIfHistoryWasDropped(path, cursor, batch, logger)
			}
			return batch, err
		})
}

// warnIfHistoryWasDropped reports a history file that exists and holds data but
// contributed no rows.
//
// The legacy-array shape did exactly this, and the only line it produced was an
// INFO saying the import had finished — so an operator's whole run history
// disappeared with nothing to read afterwards that said so. Any future shape
// this build cannot parse now says so at least once.
func warnIfHistoryWasDropped(path, cursor string, batch dbimport.Batch, logger *slog.Logger) {
	if batch.Count > 0 || cursor != "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return
	}
	logger.Warn("stats.import.no_records",
		"path", path, "bytes", info.Size(),
		"reason", "the history file holds data this build could not read; run statistics will start empty")
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

	// The shape the writer emitted before run records became NDJSON: one JSON
	// array. The file store still reads it, so a board that has not started a
	// newer build since is holding exactly this — and reading it line by line
	// finds one unparseable line and imports the operator's entire history as
	// nothing. Under a database backend nothing else ever opens the file, so
	// the rewrite that would have converted it never happens either.
	if legacy, ok, err := readLegacyArray(f); err != nil {
		return dbimport.Batch{}, err
	} else if ok {
		written := 0
		for i := range legacy {
			if legacy[i].ID == "" {
				// The same rule the ndjson path applies: a record with no id
				// would take the empty primary key, and the next such record
				// would collide with it rather than be stored.
				continue
			}
			doc, err := json.Marshal(legacy[i])
			if err != nil {
				return dbimport.Batch{}, fmt.Errorf("encode run record: %w", err)
			}
			if err := insertRunTx(ctx, database, tx, legacy[i], doc); err != nil {
				return dbimport.Batch{}, err
			}
			written++
		}
		return dbimport.Batch{Cursor: strconv.Itoa(len(legacy)), Count: written, Done: true}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return dbimport.Batch{}, fmt.Errorf("rewind run history: %w", err)
	}

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
			switch {
			case json.Unmarshal([]byte(line), &r) != nil:
				// The file store already tolerates a torn final line, which is
				// what a crash mid-append leaves. Failing here would stall the
				// import on a line it could never get past.
			case r.ID == "":
				// A line that decodes but carries no id is not a run. Inserting
				// it would take the empty primary key, and the next such line
				// would collide with it rather than be stored.
			default:
				if err := insertRunTx(ctx, database, tx, r, []byte(line)); err != nil {
					return dbimport.Batch{}, err
				}
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

// readLegacyArray decodes the pre-NDJSON whole-file JSON array, reporting
// whether the file was in that shape at all.
//
// Bounded by one document rather than batched: this shape predates the change
// that made the history append-only, so it is what a board accumulated up to
// that point and not a log that keeps growing.
func readLegacyArray(f *os.File) ([]RunRecord, bool, error) {
	head := make([]byte, 1)
	for {
		n, err := f.Read(head)
		if errors.Is(err, io.EOF) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read run history: %w", err)
		}
		if n == 0 {
			continue
		}
		if unicode.IsSpace(rune(head[0])) {
			continue
		}
		if head[0] != '[' {
			return nil, false, nil
		}
		break
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("rewind run history: %w", err)
	}
	var runs []RunRecord
	if err := json.NewDecoder(f).Decode(&runs); err != nil {
		return nil, false, fmt.Errorf("decode legacy run history: %w", err)
	}
	return runs, true, nil
}
