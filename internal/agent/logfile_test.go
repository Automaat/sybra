package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLogFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.ndjson")

	lines := `{"type":"init","content":"session started"}
{"type":"assistant","content":"hello world"}
bad line
{"type":"tool_use","content":"Read file.go"}
{"type":"result","content":"done"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseLogFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	if events[0].Type != "init" {
		t.Errorf("expected init, got %s", events[0].Type)
	}
	if events[3].Type != "result" {
		t.Errorf("expected result, got %s", events[3].Type)
	}
}

func TestParseLogFile_MaxEvents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.ndjson")

	lines := `{"type":"assistant","content":"msg1"}
{"type":"assistant","content":"msg2"}
{"type":"assistant","content":"msg3"}
{"type":"result","content":"done"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseLogFile(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (tail), got %d", len(events))
	}
	if events[0].Content != "msg3" {
		t.Errorf("expected msg3, got %s", events[0].Content)
	}
	if events[1].Content != "done" {
		t.Errorf("expected done, got %s", events[1].Content)
	}
}

func TestFindLogFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	name := "abc123-2026-04-12T10-00-00.ndjson"
	if err := os.WriteFile(filepath.Join(agentsDir, name), []byte(`{"type":"init"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindLogFile(dir, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != name {
		t.Errorf("expected %s, got %s", name, filepath.Base(got))
	}
}

func TestFindLogFile_NotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := FindLogFile(dir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing log file")
	}
}
