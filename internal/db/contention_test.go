package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
)

func TestContended_MarksEachEngineWaitShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"sqlite busy", errors.New("begin: database is locked (5) (SQLITE_BUSY)")},
		{"postgres deadlock", errors.New("ERROR: deadlock detected (SQLSTATE 40P01)")},
		{"postgres statement timeout", errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")},
		{"postgres serialization", errors.New("ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)")},
		{"postgres lock not available", errors.New("ERROR: could not obtain lock (SQLSTATE 55P03)")},
		{"statement deadline", fmt.Errorf("iterate tasks: %w", context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// When a store marks the error its statement returned
			marked := db.Contended(tc.err)

			// Then the handler recognizes it as a wait, not a fault
			if !db.IsContention(marked) {
				t.Fatalf("IsContention(Contended(%v)) = false, want true", tc.err)
			}
		})
	}
}

func TestIsContention_IgnoresAnUnmarkedRemoteTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	// Given a client timeout against a slow remote, which no store marked
	client := &http.Client{Timeout: 50 * time.Millisecond}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("the slow remote answered in time")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("client error does not wrap DeadlineExceeded: %v", err)
	}

	// Then it is not contention, so an agent never retries a dead remote forever
	if db.IsContention(err) {
		t.Fatalf("IsContention(%v) = true, want false", err)
	}
}

func TestContended_LeavesAnOrdinaryErrorAlone(t *testing.T) {
	// Given a store error that is not a wait
	err := errors.New("parse task: unexpected end of frontmatter")

	// Then passing it through leaves it a fault
	if db.IsContention(db.Contended(err)) {
		t.Fatal("an ordinary store error was classified as contention")
	}
}

func TestIsContention_IgnoresDigitsThatMerelyLookLikeAnSQLState(t *testing.T) {
	// Given a store error whose text happens to embed a SQLSTATE-shaped number
	err := errors.New("parse task: bad frontmatter at offset 40001")

	// Then it stays a fault rather than reading as a serialization failure
	if db.IsContention(db.Contended(err)) {
		t.Fatalf("IsContention classified %v as contention", err)
	}
}

func TestInTx_MarksAContendedWrite(t *testing.T) {
	// Given a write transaction whose body reports the engine made it wait
	d := openTestDB(t)
	err := d.InTx(t.Context(), func(*sql.Tx) error {
		return errors.New("write task: ERROR: deadlock detected (SQLSTATE 40P01)")
	})

	// Then the handler sees a wait, so a write contends into a retry not a 500
	if !db.IsContention(err) {
		t.Fatalf("InTx returned %v, which IsContention rejects", err)
	}
}

func TestExecContext_MarksAContendedStatement(t *testing.T) {
	// Given a statement run against a closed pool, which reports as unavailable
	d := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	_, err := d.ExecContext(ctx, "SELECT 1")

	// Then a statement that spent its deadline is a wait, not a fault
	if err != nil && !db.IsContention(err) {
		t.Fatalf("ExecContext returned %v, which IsContention rejects", err)
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), db.Options{Backend: "sqlite", DSN: filepath.Join(t.TempDir(), "c.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
