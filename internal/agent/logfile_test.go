package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// TestParseConvoLogFile_UnwrapsAnthropicEnvelope is the regression guard
// for the empty-history bug: raw Claude stream-json logs (nested message
// envelope) must surface their text/tool_use/tool_result content instead
// of being dropped by a flat StreamEvent unmarshal.
func TestParseConvoLogFile_UnwrapsAnthropicEnvelope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.ndjson")

	lines := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello from agent"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"file1\nfile2"}]}}`,
		`{"type":"result","subtype":"success","result":"done","session_id":"sess-1","total_cost_usd":0.25}`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseConvoLogFile(path, 0, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("ParseConvoLogFile: %v", err)
	}

	// We expect: init system, text assistant, tool_use assistant, tool_result user, result.
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d: %+v", len(events), events)
	}

	// Assistant text preserved.
	asstText := events[1]
	if asstText.Type != "assistant" {
		t.Errorf("event[1].Type = %s, want assistant", asstText.Type)
	}
	if asstText.Text != "hello from agent" {
		t.Errorf("event[1].Text = %q, want %q", asstText.Text, "hello from agent")
	}

	// Tool use preserved with name + input.
	asstTool := events[2]
	if len(asstTool.ToolUses) != 1 {
		t.Fatalf("event[2].ToolUses len = %d, want 1", len(asstTool.ToolUses))
	}
	if asstTool.ToolUses[0].Name != "Bash" {
		t.Errorf("tool_use.Name = %q, want Bash", asstTool.ToolUses[0].Name)
	}
	if asstTool.ToolUses[0].ID != "tu-1" {
		t.Errorf("tool_use.ID = %q, want tu-1", asstTool.ToolUses[0].ID)
	}

	// Tool result paired with the tool_use_id.
	userResult := events[3]
	if userResult.Type != "user" {
		t.Errorf("event[3].Type = %s, want user", userResult.Type)
	}
	if len(userResult.ToolResults) != 1 {
		t.Fatalf("event[3].ToolResults len = %d, want 1", len(userResult.ToolResults))
	}
	if userResult.ToolResults[0].ToolUseID != "tu-1" {
		t.Errorf("tool_result.ToolUseID = %q, want tu-1", userResult.ToolResults[0].ToolUseID)
	}
	if !strings.Contains(userResult.ToolResults[0].Content, "file1") {
		t.Errorf("tool_result.Content missing output: %q", userResult.ToolResults[0].Content)
	}

	// Terminal result event carries cost + session.
	res := events[4]
	if res.Type != "result" {
		t.Errorf("event[4].Type = %s, want result", res.Type)
	}
	if res.CostUSD != 0.25 {
		t.Errorf("result.CostUSD = %v, want 0.25", res.CostUSD)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("result.SessionID = %q, want sess-1", res.SessionID)
	}
}

func TestParseConvoLogFile_MaxEventsKeepsTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.ndjson")

	var sb strings.Builder
	for range 10 {
		sb.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"`)
		sb.WriteString("msg-")
		sb.WriteString(strings.Repeat("x", 1))
		sb.WriteString("\"}]}}\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseConvoLogFile(path, 3, nil)
	if err != nil {
		t.Fatalf("ParseConvoLogFile: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (tail), got %d", len(events))
	}
}

func TestParseConvoLogFile_SkipsMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.ndjson")

	lines := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"good"}]}}`,
		`not json at all`,
		``,
		`{"type":""}`,
		`{"type":"result","subtype":"success","result":"ok"}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseConvoLogFile(path, 0, nil)
	if err != nil {
		t.Fatalf("ParseConvoLogFile: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (malformed skipped), got %d", len(events))
	}
	if events[0].Text != "good" {
		t.Errorf("events[0].Text = %q, want good", events[0].Text)
	}
	if events[1].Type != "result" {
		t.Errorf("events[1].Type = %q, want result", events[1].Type)
	}
}

func TestParseConvoLogFile_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := ParseConvoLogFile(filepath.Join(t.TempDir(), "missing.ndjson"), 0, nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
