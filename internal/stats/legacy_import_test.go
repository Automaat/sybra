package stats

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestImport_LegacyJSONArrayHistory pins the shape the writer emitted before
// run records became NDJSON. The file store still reads it, so a board that has
// not started a newer build since still holds exactly this file — and importing
// it as nothing loses the operator's whole run history, silently and for good.
func TestImport_LegacyJSONArrayHistory(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		runs := []RunRecord{
			{ID: "legacy-0", TaskID: "t-0", Mode: "headless", Role: "implementation", CostUSD: 2, Timestamp: now},
			{ID: "legacy-1", TaskID: "t-1", Mode: "headless", Role: "review", CostUSD: 4, Timestamp: now.Add(time.Hour)},
		}
		data, err := json.Marshal(runs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(t.TempDir(), "stats.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		// Import runs first, as it does in production: under a database backend
		// the file store is never opened, so nothing rewrites the legacy shape
		// into NDJSON before the import reads it. Opening the store here would
		// convert the file and hide the defect entirely.
		if err := Import(t.Context(), d, path, "home-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		sqlStore, err := NewSQLStore(d, nil)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if n := sqlStore.Len(); n != 2 {
			t.Fatalf("import moved %d records, want 2; the operator's history was dropped", n)
		}

		// A separate copy confirms the file store really does read this shape,
		// which is why a board can still be holding it.
		copyPath := filepath.Join(t.TempDir(), "stats.json")
		if err := os.WriteFile(copyPath, data, 0o600); err != nil {
			t.Fatalf("write copy: %v", err)
		}
		fileStore, err := NewStore(copyPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if n := fileStore.Len(); n != 2 {
			t.Fatalf("the file store read %d records from the legacy shape, want 2", n)
		}
	})
}

// TestImport_WarnsWhenAHistoryFileContributesNothing pins the second half of
// the defect: the loss was silent. The only line the legacy shape produced was
// an INFO saying the import had finished, so nothing an operator could read
// afterwards said their history had gone.
func TestImport_WarnsWhenAHistoryFileContributesNothing(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "stats.json")
		// A shape no build reads: not NDJSON records, not the legacy array.
		if err := os.WriteFile(path, []byte("not json at all\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

		if err := Import(t.Context(), d, path, "home-a", logger); err != nil {
			t.Fatalf("import: %v", err)
		}
		if !strings.Contains(buf.String(), "stats.import.no_records") {
			t.Fatalf("a history file that contributed nothing was imported silently; log was %q", buf.String())
		}
	})
}
