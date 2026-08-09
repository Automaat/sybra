package monitor

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func outboxFixture() []outboxItem {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []outboxItem{
		{Fingerprint: "fp-b", Operation: "create", Title: "second", Attempts: 2, FirstFailedAt: base.Add(time.Hour)},
		{Fingerprint: "fp-a", Operation: "create", Title: "first", Attempts: 1, FirstFailedAt: base},
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestSQLIssueOutbox_RoundTripsAndDrainsOldestFirst pins what the outbox is
// for: an entry survives the crash between deciding to file and GitHub
// accepting it, and the retry loop drains in the order entries were stranded.
func TestSQLIssueOutbox_RoundTripsAndDrainsOldestFirst(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLIssueOutbox(d, discardLog())
		if err != nil {
			t.Fatalf("NewSQLIssueOutbox: %v", err)
		}
		for _, it := range outboxFixture() {
			if err := store.put(it); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
		got := store.load(discardLog())
		if len(got) != 2 {
			t.Fatalf("outbox holds %d items, want 2", len(got))
		}
		if got[0].Fingerprint != "fp-a" {
			t.Errorf("drain order starts at %q, want the oldest failure fp-a", got[0].Fingerprint)
		}
		if store.depth() != 2 {
			t.Errorf("depth = %d, want 2", store.depth())
		}

		one, ok := store.get("fp-b")
		if !ok || one.Title != "second" {
			t.Fatalf("get(fp-b) = %+v ok=%v", one, ok)
		}

		if err := store.del("fp-a"); err != nil {
			t.Fatalf("del: %v", err)
		}
		if store.depth() != 1 {
			t.Errorf("after delete depth = %d, want 1", store.depth())
		}
		if _, ok := store.get("fp-a"); ok {
			t.Error("a filed entry came back after deletion")
		}
	})
}

// TestImportIssueOutbox_IsOnceOnlyAndLeavesTheFiles pins the upgrade
// guarantees. A duplicated entry here files the same issue twice.
func TestImportIssueOutbox_IsOnceOnlyAndLeavesTheFiles(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		fileStore, err := newIssueOutboxStore(dir)
		if err != nil {
			t.Fatalf("newIssueOutboxStore: %v", err)
		}
		for _, it := range outboxFixture() {
			if err := fileStore.put(it); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
		for range 2 {
			if err := ImportIssueOutbox(t.Context(), d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLIssueOutbox(d, discardLog())
		if err != nil {
			t.Fatalf("NewSQLIssueOutbox: %v", err)
		}
		if n := store.depth(); n != 2 {
			t.Fatalf("after two imports the outbox holds %d entries, want 2", n)
		}
		for _, it := range outboxFixture() {
			if _, err := os.Stat(filepath.Join(dir, it.Fingerprint+".yaml")); err != nil {
				t.Errorf("import removed %s: %v", it.Fingerprint, err)
			}
		}
	})
}
