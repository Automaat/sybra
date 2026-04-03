package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()

	files := []string{
		"2026-03-01.ndjson", // old — should be removed
		"2026-03-30.ndjson", // old — should be removed
		"2026-04-02.ndjson", // recent — keep
		"2026-04-03.ndjson", // today — keep
		"notes.txt",         // not ndjson — keep
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

	if names["2026-03-01.ndjson"] {
		t.Error("old file 2026-03-01 not removed")
	}
	if names["2026-03-30.ndjson"] {
		t.Error("old file 2026-03-30 not removed")
	}
	if !names["2026-04-02.ndjson"] {
		t.Error("recent file 2026-04-02 removed")
	}
	if !names["2026-04-03.ndjson"] {
		t.Error("today file removed")
	}
	if !names["notes.txt"] {
		t.Error("non-ndjson file removed")
	}
}

func TestCleanupNonexistentDir(t *testing.T) {
	if err := Cleanup("/nonexistent/path", 30); err != nil {
		t.Errorf("expected nil error for nonexistent dir, got %v", err)
	}
}
