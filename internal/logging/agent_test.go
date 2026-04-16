package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewAgentOutputFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentID := "abc12345"

	f, err := NewAgentOutputFile(dir, agentID)
	if err != nil {
		t.Fatalf("NewAgentOutputFile: %v", err)
	}
	defer f.Close()

	if !strings.Contains(f.Name(), agentID) {
		t.Errorf("filename %q does not contain agentID %q", f.Name(), agentID)
	}
	if !strings.HasSuffix(f.Name(), ".ndjson") {
		t.Errorf("filename %q does not end with .ndjson", f.Name())
	}
	// File should be inside the agents/ subdirectory.
	agentsDir := filepath.Join(dir, "agents")
	if !strings.HasPrefix(f.Name(), agentsDir) {
		t.Errorf("file %q not under agents/ dir %q", f.Name(), agentsDir)
	}
}

func TestNewAgentOutputFile_CreatesDir(t *testing.T) {
	t.Parallel()
	// Use a subdirectory that doesn't exist yet.
	dir := filepath.Join(t.TempDir(), "logs", "nested")

	f, err := NewAgentOutputFile(dir, "agt-001")
	if err != nil {
		t.Fatalf("NewAgentOutputFile: %v", err)
	}
	defer f.Close()

	agentsDir := filepath.Join(dir, "agents")
	if _, err := os.Stat(agentsDir); err != nil {
		t.Errorf("agents/ dir not created: %v", err)
	}
}

func TestPruneAgentLogs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	// Table-driven covers the three outcomes PruneAgentLogs can produce for
	// a single file: delete because empty, delete because old, keep.
	// Each case writes one file, runs the sweep, and asserts the report.
	cases := []struct {
		name      string
		size      int
		ageDays   int
		maxAge    time.Duration
		wantOld   int
		wantEmpty int
		wantKept  int
	}{
		{"empty file always deleted", 0, 0, 14 * 24 * time.Hour, 0, 1, 0},
		{"empty file deleted when retention disabled", 0, 0, 0, 0, 1, 0},
		{"old non-empty file deleted", 100, 30, 14 * 24 * time.Hour, 1, 0, 0},
		{"fresh non-empty file kept", 100, 1, 14 * 24 * time.Hour, 0, 0, 1},
		{"old file kept when retention disabled", 100, 30, 0, 0, 0, 1},
		{"exactly-at-cutoff file kept", 100, 14, 14 * 24 * time.Hour, 0, 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			agents := filepath.Join(dir, "agents")
			if err := os.MkdirAll(agents, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			path := filepath.Join(agents, "f-"+time.Now().Format("150405.000000000")+".ndjson")
			if err := os.WriteFile(path, make([]byte, tc.size), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			mtime := now.Add(-time.Duration(tc.ageDays) * 24 * time.Hour)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			r := PruneAgentLogs(dir, tc.maxAge, now)
			if r.DeletedOld != tc.wantOld {
				t.Errorf("DeletedOld=%d want %d", r.DeletedOld, tc.wantOld)
			}
			if r.DeletedEmpty != tc.wantEmpty {
				t.Errorf("DeletedEmpty=%d want %d", r.DeletedEmpty, tc.wantEmpty)
			}
			if r.Kept != tc.wantKept {
				t.Errorf("Kept=%d want %d", r.Kept, tc.wantKept)
			}
			if len(r.Errors) != 0 {
				t.Errorf("unexpected errors: %v", r.Errors)
			}

			// The filesystem is the source of truth — verify the kept/deleted
			// outcome matches what the report claimed, catching any report/
			// effect divergence the unit-level comparison would miss.
			_, statErr := os.Stat(path)
			if tc.wantKept == 1 && statErr != nil {
				t.Errorf("kept file removed: %v", statErr)
			}
			if (tc.wantOld+tc.wantEmpty) == 1 && !os.IsNotExist(statErr) {
				t.Errorf("deleted file still present, stat err=%v", statErr)
			}
		})
	}
}

func TestPruneAgentLogs_SkipsNonNDJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A debris file that predates this change — sweep must ignore it
	// even though it is empty and old.
	decoy := filepath.Join(agents, "notes.txt")
	if err := os.WriteFile(decoy, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := PruneAgentLogs(dir, 24*time.Hour, time.Now())
	if r.Scanned != 0 {
		t.Errorf("non-ndjson file should not be scanned, got %d", r.Scanned)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("decoy file was removed: %v", err)
	}
}

func TestPruneAgentLogs_MissingDirIsNoop(t *testing.T) {
	t.Parallel()
	r := PruneAgentLogs(filepath.Join(t.TempDir(), "nope"), 24*time.Hour, time.Now())
	if r.Scanned != 0 || len(r.Errors) != 0 {
		t.Errorf("expected empty report for missing dir, got %+v", r)
	}
}

func TestPruneAgentLogs_EmptyLogDirIsNoop(t *testing.T) {
	t.Parallel()
	r := PruneAgentLogs("", 24*time.Hour, time.Now())
	if r.Scanned != 0 || len(r.Errors) != 0 {
		t.Errorf("expected empty report for empty logDir, got %+v", r)
	}
}
