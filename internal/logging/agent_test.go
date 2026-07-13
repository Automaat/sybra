package logging

import (
	"compress/gzip"
	"io"
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

func writeAgentLog(t *testing.T, agentsDir, name, content string, age time.Duration, now time.Time) string {
	t.Helper()
	path := filepath.Join(agentsDir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mtime := now.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

func TestEnforceAgentLogRetention_Gzip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := strings.Repeat("hello world\n", 100)
	old := writeAgentLog(t, agents, "agt-1-2026-04-01T00-00-00.ndjson", content, 5*24*time.Hour, now)
	fresh := writeAgentLog(t, agents, "agt-2-2026-04-16T00-00-00.ndjson", content, time.Hour, now)

	r := EnforceAgentLogRetention(dir, RetentionOptions{GzipAfter: 3 * 24 * time.Hour}, now)
	if r.Compressed != 1 {
		t.Errorf("Compressed = %d, want 1", r.Compressed)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("original old file should have been removed, stat err=%v", err)
	}
	gzPath := old + ".gz"
	f, err := os.Open(gzPath)
	if err != nil {
		t.Fatalf("open .gz sibling: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip content: %v", err)
	}
	if string(got) != content {
		t.Errorf("decompressed content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file should be untouched: %v", err)
	}
}

func TestEnforceAgentLogRetention_GzipPreservesAgeForLaterPrune(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := writeAgentLog(t, agents, "agt-1-2026-04-01T00-00-00.ndjson", "old\n", 10*24*time.Hour, now)

	first := EnforceAgentLogRetention(dir, RetentionOptions{
		MaxAge:    14 * 24 * time.Hour,
		GzipAfter: 3 * 24 * time.Hour,
	}, now)
	if first.Compressed != 1 {
		t.Fatalf("first pass Compressed = %d, want 1", first.Compressed)
	}

	gzPath := old + ".gz"
	info, err := os.Stat(gzPath)
	if err != nil {
		t.Fatalf("stat .gz sibling: %v", err)
	}
	if !info.ModTime().Equal(now.Add(-10 * 24 * time.Hour)) {
		t.Fatalf("compressed mtime = %s, want %s", info.ModTime(), now.Add(-10*24*time.Hour))
	}

	second := EnforceAgentLogRetention(dir, RetentionOptions{
		MaxAge: 14 * 24 * time.Hour,
	}, now.Add(5*24*time.Hour))
	if second.DeletedOld != 1 {
		t.Fatalf("second pass DeletedOld = %d, want 1", second.DeletedOld)
	}
	if _, err := os.Stat(gzPath); !os.IsNotExist(err) {
		t.Fatalf("compressed file should be age-pruned later, stat err=%v", err)
	}
}

func TestEnforceAgentLogRetention_SizeCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	chunk := strings.Repeat("x", 100)
	oldest := writeAgentLog(t, agents, "agt-1.ndjson", chunk, 3*time.Hour, now)
	middle := writeAgentLog(t, agents, "agt-2.ndjson", chunk, 2*time.Hour, now)
	newest := writeAgentLog(t, agents, "agt-3.ndjson", chunk, time.Hour, now)

	// Total is 300 bytes; cap at 150 should evict the two oldest files,
	// oldest-mtime-first, and keep the newest.
	r := EnforceAgentLogRetention(dir, RetentionOptions{MaxTotalBytes: 150}, now)
	if r.DeletedForSize != 2 {
		t.Errorf("DeletedForSize = %d, want 2", r.DeletedForSize)
	}
	if r.Kept != 1 {
		t.Errorf("Kept = %d, want 1", r.Kept)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest file should have been evicted, stat err=%v", err)
	}
	if _, err := os.Stat(middle); !os.IsNotExist(err) {
		t.Errorf("middle file should have been evicted, stat err=%v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("newest file should survive: %v", err)
	}
}

func TestEnforceAgentLogRetention_ProtectsActiveLogPaths(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	chunk := strings.Repeat("x", 100)
	// Old, empty-eligible, and over-cap on every axis — would be deleted by
	// every pass if it weren't for the active-log protection.
	active := writeAgentLog(t, agents, "agt-active.ndjson", chunk, 30*24*time.Hour, now)
	activeStderr := writeAgentLog(t, agents, "agt-active.ndjson.stderr", "", 30*24*time.Hour, now)

	r := EnforceAgentLogRetention(dir, RetentionOptions{
		MaxAge:         24 * time.Hour,
		GzipAfter:      time.Hour,
		MaxTotalBytes:  1,
		ActiveLogPaths: map[string]bool{active: true},
	}, now)

	if r.Protected != 2 {
		t.Errorf("Protected = %d, want 2 (main log + stderr sidecar)", r.Protected)
	}
	if r.DeletedOld != 0 || r.DeletedEmpty != 0 || r.DeletedForSize != 0 || r.Compressed != 0 {
		t.Errorf("expected no deletions/compression of protected files, got %+v", r)
	}
	if _, err := os.Stat(active); err != nil {
		t.Errorf("active log was removed/compressed: %v", err)
	}
	if _, err := os.Stat(activeStderr); err != nil {
		t.Errorf("active stderr sidecar was removed: %v", err)
	}
}
