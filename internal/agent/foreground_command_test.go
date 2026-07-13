package agent

import (
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
