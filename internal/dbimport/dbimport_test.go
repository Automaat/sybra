package dbimport

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestOnce_RunsExactlyOnce is the "second start does not duplicate them"
// guarantee every domain's import relies on.
func TestOnce_RunsExactlyOnce(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		calls := 0
		for range 3 {
			if err := Once(t.Context(), d, "demo", "home-a", nil, func(context.Context, *sql.Tx) (int, error) {
				calls++
				return 7, nil
			}); err != nil {
				t.Fatalf("once: %v", err)
			}
		}
		if calls != 1 {
			t.Fatalf("import body ran %d times, want 1", calls)
		}
	})
}

// TestOnce_InterruptedImportRetriesCleanly is the issue's hardest guarantee.
//
// The marker is written in the same transaction as the rows, so a body that
// fails halfway must leave neither: no marker (or the domain never retries) and
// no rows (or the retry duplicates everything it already wrote).
func TestOnce_InterruptedImportRetriesCleanly(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		ctx := t.Context()
		boom := errors.New("interrupted")
		err := Once(ctx, d, "demo", "home-a", nil, func(ctx context.Context, tx *sql.Tx) (int, error) {
			if _, err := tx.ExecContext(ctx, d.Rebind(
				`INSERT INTO experience_records (project_key, record_id, created_at, doc) VALUES (?, ?, ?, ?)`),
				"proj", "half-written", int64(0), "{}"); err != nil {
				return 0, err
			}
			return 0, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("interrupted import returned %v, want the body's error", err)
		}

		done, err := Imported(ctx, d, "demo", "home-a")
		if err != nil {
			t.Fatalf("imported: %v", err)
		}
		if done {
			t.Fatal("an interrupted import left a marker; the domain would never retry")
		}

		var rows int
		if err := d.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM experience_records WHERE record_id = ?`, "half-written").Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}
		if rows != 0 {
			t.Fatalf("an interrupted import committed %d rows; the retry would duplicate them", rows)
		}

		calls := 0
		if err := Once(ctx, d, "demo", "home-a", nil, func(context.Context, *sql.Tx) (int, error) {
			calls++
			return 1, nil
		}); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if calls != 1 {
			t.Fatalf("retry ran the body %d times, want 1", calls)
		}
	})
}

// TestOnce_DomainsAreIndependent keeps one domain's marker from standing in for
// another's — every domain imports on its own schedule.
func TestOnce_DomainsAreIndependent(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		ran := map[string]int{}
		for _, domain := range []string{"alpha", "beta"} {
			for range 2 {
				if err := Once(t.Context(), d, domain, "home-a", nil, func(context.Context, *sql.Tx) (int, error) {
					ran[domain]++
					return 0, nil
				}); err != nil {
					t.Fatalf("once %s: %v", domain, err)
				}
			}
		}
		if ran["alpha"] != 1 || ran["beta"] != 1 {
			t.Fatalf("ran %v, want each domain exactly once", ran)
		}
	})
}

// TestOnce_ScopesAreIndependent is the shared-board case.
//
// A postgres board is shared by several machines, each with its own home and
// its own files. Keyed by domain alone, whichever instance started first would
// claim the domain and every other machine's records would sit unimported
// forever, with nothing reporting it.
func TestOnce_ScopesAreIndependent(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		ran := map[string]int{}
		for _, scope := range []string{"home-a", "home-b"} {
			for range 2 {
				if err := Once(t.Context(), d, "demo", scope, nil, func(context.Context, *sql.Tx) (int, error) {
					ran[scope]++
					return 0, nil
				}); err != nil {
					t.Fatalf("once %s: %v", scope, err)
				}
			}
		}
		if ran["home-a"] != 1 || ran["home-b"] != 1 {
			t.Fatalf("ran %v, want each home imported exactly once", ran)
		}
	})
}

func TestOnce_RefusesAnEmptyScope(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Once(t.Context(), d, "demo", "  ", nil, func(context.Context, *sql.Tx) (int, error) {
			return 0, nil
		}); err == nil {
			t.Fatal("accepted a blank scope; every home would share one marker")
		}
	})
}

// TestResumable_PartialImportResumesFromItsCursor is the issue's "a partial
// import resumes without duplicating rows".
//
// Running an import twice to completion does not test this: the second run
// reads done=1 and returns before calling next at all, so such a test passes
// even if the cursor is never persisted, never read, or reset on every start.
// Here the first run fails mid-way, and the second must pick up where it
// stopped rather than restarting from nothing.
func TestResumable_PartialImportResumesFromItsCursor(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		ctx := t.Context()
		boom := errors.New("interrupted")

		var seen []string
		next := func(stopAt int) func(context.Context, *sql.Tx, string) (Batch, error) {
			return func(_ context.Context, _ *sql.Tx, cursor string) (Batch, error) {
				seen = append(seen, cursor)
				n := 0
				if cursor != "" {
					n = len(cursor)
				}
				if n == stopAt {
					return Batch{}, boom
				}
				if n >= 4 {
					return Batch{Cursor: cursor, Count: 1, Done: true}, nil
				}
				return Batch{Cursor: cursor + "x", Count: 1}, nil
			}
		}

		if err := Resumable(ctx, d, "demo", "home-a", nil, next(3)); !errors.Is(err, boom) {
			t.Fatalf("first run returned %v, want the body's error", err)
		}
		if len(seen) == 0 || seen[0] != "" {
			t.Fatalf("first run started at %q, want the empty cursor", seen)
		}
		firstRun := append([]string(nil), seen...)
		seen = nil

		if err := Resumable(ctx, d, "demo", "home-a", nil, next(-1)); err != nil {
			t.Fatalf("second run: %v", err)
		}
		if len(seen) == 0 {
			t.Fatal("second run never called next; the domain was marked done by an interrupted import")
		}
		if seen[0] == "" {
			t.Fatalf("second run restarted from nothing (%v) after %v; the cursor was not read back", seen, firstRun)
		}
		if want := firstRun[len(firstRun)-1]; seen[0] != want {
			t.Fatalf("second run resumed at %q, want the last committed cursor %q", seen[0], want)
		}
	})
}

// TestResumable_RefusesABatchThatDoesNotAdvance keeps a defective domain from
// spinning forever while holding the import lock, which would block every
// other domain's import with nothing in the log to say why.
func TestResumable_RefusesABatchThatDoesNotAdvance(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		err := Resumable(t.Context(), d, "demo", "home-a", nil,
			func(context.Context, *sql.Tx, string) (Batch, error) {
				return Batch{Cursor: "", Count: 1}, nil
			})
		if err == nil {
			t.Fatal("a batch that never advances was accepted; the import spins forever")
		}
	})
}
