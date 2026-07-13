package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyStreamEventState_TracksForegroundBashLifecycle(t *testing.T) {
	a := &Agent{}
	startedAt := time.Unix(1700000000, 0).UTC()

	a.applyStreamEventState(StreamEvent{
		Type:      "assistant",
		Timestamp: startedAt,
		toolUses: []ToolUseBlock{{
			ID:    "tu-1",
			Name:  "Bash",
			Input: map[string]any{"command": "mise run verify"},
		}},
	})

	command, gotStartedAt, ok := a.ActiveForegroundCommand()
	if !ok {
		t.Fatal("ActiveForegroundCommand() = not found, want live command")
	}
	if command != "mise run verify" {
		t.Fatalf("command = %q, want %q", command, "mise run verify")
	}
	if !gotStartedAt.Equal(startedAt) {
		t.Fatalf("startedAt = %s, want %s", gotStartedAt, startedAt)
	}

	a.applyStreamEventState(StreamEvent{
		Type: "user",
		toolResults: []ToolResultBlock{{
			ToolUseID: "tu-1",
			Content:   "ok",
		}},
	})

	if command, _, ok := a.ActiveForegroundCommand(); ok {
		t.Fatalf("ActiveForegroundCommand() = %q, want cleared after tool_result", command)
	}
}

func TestApplyStreamEventState_IgnoresNonBashToolUses(t *testing.T) {
	a := &Agent{}

	a.applyStreamEventState(StreamEvent{
		Type:      "assistant",
		Timestamp: time.Unix(1700000100, 0).UTC(),
		toolUses: []ToolUseBlock{{
			ID:    "tu-1",
			Name:  "Read",
			Input: map[string]any{"file": "README.md"},
		}},
	})

	if command, _, ok := a.ActiveForegroundCommand(); ok {
		t.Fatalf("ActiveForegroundCommand() = %q, want no foreground command for non-Bash tool", command)
	}
}

func TestRehydrateFromLog_RebuildsForegroundBashState(t *testing.T) {
	t.Run("live command remains active", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent.ndjson")
		data := strings.Join([]string{
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"mise run verify"}}]}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}

		a := &Agent{Provider: "claude"}
		rehydrateFromLog(a, path)

		command, _, ok := a.ActiveForegroundCommand()
		if !ok {
			t.Fatal("ActiveForegroundCommand() = not found after rehydrate, want live command")
		}
		if command != "mise run verify" {
			t.Fatalf("command = %q, want %q", command, "mise run verify")
		}
	})

	t.Run("matching tool result clears command", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent.ndjson")
		data := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"mise run verify"}}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"ok"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}

		a := &Agent{Provider: "claude"}
		rehydrateFromLog(a, path)

		if command, _, ok := a.ActiveForegroundCommand(); ok {
			t.Fatalf("ActiveForegroundCommand() = %q after rehydrate, want cleared by tool_result", command)
		}
	})
}
