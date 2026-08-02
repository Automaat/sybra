package toolledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLogRecordsRegardlessOfDecision pins the ledger's reason for existing: it
// records observations and adjudications alike. A store that only captured
// refusals would describe what a human rejected and nothing about the far
// larger set they approved — the wrong half for deriving a policy.
func TestLogRecordsRegardlessOfDecision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ts := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{Timestamp: ts, AgentID: "a1", Tool: "Bash", Input: map[string]any{"command": "go test ./..."}},
		{Timestamp: ts, AgentID: "a1", Tool: "Write", Decision: "allow", DecidedBy: "human"},
		{Timestamp: ts, AgentID: "a1", Tool: "Bash", Decision: "deny", DecidedBy: "human"},
		{Timestamp: ts, AgentID: "a1", Tool: "Read", Decision: "allow", DecidedBy: "safe-tool"},
	}
	for _, r := range records {
		if err := l.Log(r); err != nil {
			t.Fatalf("Log(%s): %v", r.Tool, err)
		}
	}

	got := readAll(t, filepath.Join(dir, "2026-08-02.ndjson"))
	if len(got) != len(records) {
		t.Fatalf("wrote %d records, want %d", len(got), len(records))
	}
	if got[0].Decision != "" {
		t.Errorf("observation carries decision %q, want empty", got[0].Decision)
	}
	if got[1].Decision != "allow" || got[1].DecidedBy != "human" {
		t.Errorf("approval = %q by %q, want allow by human", got[1].Decision, got[1].DecidedBy)
	}
	if got[2].Decision != "deny" {
		t.Errorf("refusal = %q, want deny", got[2].Decision)
	}
	if got[3].DecidedBy != "safe-tool" {
		t.Errorf("automatic approval attributed to %q, want safe-tool", got[3].DecidedBy)
	}
}

// TestLogFilesByUTCDate pins the same discipline internal/audit needed: the
// file name comes from the record's date, so a caller stamping a local-zone
// timestamp must not file it under a date a UTC reader never looks for.
func TestLogFilesByUTCDate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// 00:30 on Aug 2 in CEST is still 22:30 on Aug 1 in UTC.
	local := time.Date(2026, 8, 2, 0, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	if err := l.Log(Record{Timestamp: local, AgentID: "a1", Tool: "Bash"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-01.ndjson")); err != nil {
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("filed as %v, want 2026-08-01.ndjson — the caller's zone must not name the file", names)
	}
}

// TestNilLoggerIsSafe matters because the sink sits on the agent stream path,
// where a missing ledger must not take the run down with it.
func TestNilLoggerIsSafe(t *testing.T) {
	t.Parallel()
	var l *Logger
	if err := l.Log(Record{Tool: "Bash"}); err != nil {
		t.Fatalf("Log on nil Logger: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close on nil Logger: %v", err)
	}
}

func readAll(t *testing.T, path string) []Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var out []Record
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var r Record
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out = append(out, r)
	}
	return out
}
