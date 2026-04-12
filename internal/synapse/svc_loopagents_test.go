package synapse

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanAuditFileForRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-04-11.ndjson")

	content := `{"ts":"2026-04-11T10:00:00Z","type":"agent.started","agent_id":"a1","data":{"title":"loop:self-monitor"}}
{"ts":"2026-04-11T10:05:00Z","type":"agent.completed","agent_id":"a1","data":{"name":"loop:self-monitor","cost_usd":0.08,"duration_s":300,"state":"stopped"}}
{"ts":"2026-04-11T11:00:00Z","type":"agent.completed","agent_id":"a2","data":{"name":"loop:self-monitor","cost_usd":0.12,"duration_s":600,"state":"stopped"}}
{"ts":"2026-04-11T12:00:00Z","type":"agent.completed","agent_id":"a3","data":{"name":"loop:other-loop","cost_usd":0.50,"duration_s":120,"state":"stopped"}}
{"ts":"2026-04-11T13:00:00Z","type":"agent.completed","agent_id":"a4","data":{"name":"triage:My Task","cost_usd":0.10,"duration_s":30,"state":"stopped"}}
{"ts":"2026-04-11T14:00:00Z","type":"task.status_changed","task_id":"t1","data":{"from":"todo","to":"in-progress"}}
{"this is not json at all"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := scanAuditFileForRuns(path, "loop:self-monitor")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(runs) != 2 {
		t.Fatalf("expected 2 runs for loop:self-monitor, got %d", len(runs))
	}

	// Verify first run
	r0 := runs[0]
	if r0.AgentID != "a1" {
		t.Errorf("run[0].AgentID = %q, want a1", r0.AgentID)
	}
	if r0.CostUSD != 0.08 {
		t.Errorf("run[0].CostUSD = %v, want 0.08", r0.CostUSD)
	}
	if r0.DurationS != 300 {
		t.Errorf("run[0].DurationS = %v, want 300", r0.DurationS)
	}
	if r0.State != "stopped" {
		t.Errorf("run[0].State = %q, want stopped", r0.State)
	}

	// Verify second run
	r1 := runs[1]
	if r1.AgentID != "a2" || r1.CostUSD != 0.12 {
		t.Errorf("run[1] mismatch: id=%q cost=%v", r1.AgentID, r1.CostUSD)
	}

	// Different loop name should not match
	otherRuns, err := scanAuditFileForRuns(path, "loop:other-loop")
	if err != nil {
		t.Fatalf("scan other: %v", err)
	}
	if len(otherRuns) != 1 || otherRuns[0].AgentID != "a3" {
		t.Fatalf("other: expected 1 run for a3, got %d", len(otherRuns))
	}

	// Exact name matching — different name returns nothing
	noRuns, err := scanAuditFileForRuns(path, "loop:nonexistent")
	if err != nil {
		t.Fatalf("scan nonexistent: %v", err)
	}
	if len(noRuns) != 0 {
		t.Fatalf("expected 0 runs for nonexistent loop, got %d", len(noRuns))
	}

	// Exact match on triage name works (function matches any name, not only loop: prefixed)
	triageRuns, err := scanAuditFileForRuns(path, "triage:My Task")
	if err != nil {
		t.Fatalf("scan triage: %v", err)
	}
	if len(triageRuns) != 1 || triageRuns[0].AgentID != "a4" {
		t.Fatalf("expected 1 triage run for a4, got %d", len(triageRuns))
	}
}

func TestScanAuditFileForRuns_MissingFile(t *testing.T) {
	_, err := scanAuditFileForRuns("/nonexistent/file.ndjson", "loop:x")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestScanAuditFileForRuns_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.ndjson")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, err := scanAuditFileForRuns(path, "loop:x")
	if err != nil {
		t.Fatalf("scan empty: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs, got %d", len(runs))
	}
}

func TestAuditFilesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"2026-04-09.ndjson", "2026-04-11.ndjson", "2026-04-10.ndjson", "not-audit.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := auditFilesNewestFirst(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 ndjson files, got %d", len(files))
	}
	// Newest first
	if filepath.Base(files[0]) != "2026-04-11.ndjson" {
		t.Errorf("files[0] = %s, want 2026-04-11.ndjson", filepath.Base(files[0]))
	}
	if filepath.Base(files[2]) != "2026-04-09.ndjson" {
		t.Errorf("files[2] = %s, want 2026-04-09.ndjson", filepath.Base(files[2]))
	}
}

func TestAuditFilesNewestFirst_MissingDir(t *testing.T) {
	files, err := auditFilesNewestFirst("/nonexistent/dir")
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}
