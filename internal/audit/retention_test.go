package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRemovesOldFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Now().UTC()
	today := now.Format(time.DateOnly)
	yesterday := now.AddDate(0, 0, -1).Format(time.DateOnly)
	old5 := now.AddDate(0, 0, -5).Format(time.DateOnly)
	old30 := now.AddDate(0, 0, -30).Format(time.DateOnly)

	files := []string{
		old30 + ".ndjson",     // old — should be removed
		old5 + ".ndjson",      // old — should be removed
		yesterday + ".ndjson", // recent — keep
		today + ".ndjson",     // today — keep
		"notes.txt",           // not ndjson — keep
	}
	for _, f := range files {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644)
	}

	if err := Cleanup(dir, 3); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if names[old30+".ndjson"] {
		t.Error("old file not removed:", old30)
	}
	if names[old5+".ndjson"] {
		t.Error("old file not removed:", old5)
	}
	if !names[yesterday+".ndjson"] {
		t.Error("recent file removed:", yesterday)
	}
	if !names[today+".ndjson"] {
		t.Error("today file removed:", today)
	}
	if !names["notes.txt"] {
		t.Error("non-ndjson file removed")
	}
}

func TestCleanupNonexistentDir(t *testing.T) {
	t.Parallel()
	if err := Cleanup("/nonexistent/path", 30); err != nil {
		t.Errorf("expected nil error for nonexistent dir, got %v", err)
	}
}
