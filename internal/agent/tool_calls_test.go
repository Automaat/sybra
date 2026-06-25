package agent

import "testing"

func TestAgentToolCalls(t *testing.T) {
	a := &Agent{}
	if got := a.GetToolCalls(); got != 0 {
		t.Fatalf("initial GetToolCalls = %d, want 0", got)
	}
	a.AddToolCalls(3)
	a.AddToolCalls(2)
	a.AddToolCalls(0)  // no-op
	a.AddToolCalls(-5) // no-op, never decrements
	if got := a.GetToolCalls(); got != 5 {
		t.Errorf("GetToolCalls = %d, want 5", got)
	}
}

func TestClaudeEventToStreamEvent_ToolCalls(t *testing.T) {
	tests := []struct {
		name string
		ev   ClaudeEvent
		want int
	}{
		{
			name: "assistant with two tool uses",
			ev: ClaudeEvent{Type: "assistant", Message: &ClaudeMessage{
				ToolUses: []ToolUseBlock{{Name: "Read"}, {Name: "Bash"}},
			}},
			want: 2,
		},
		{
			name: "assistant text only",
			ev:   ClaudeEvent{Type: "assistant", Message: &ClaudeMessage{Text: "hi"}},
			want: 0,
		},
		{
			name: "result event carries no tool calls",
			ev:   ClaudeEvent{Type: "result", Result: &ClaudeResult{Text: "done"}},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeEventToStreamEvent(tt.ev).ToolCalls; got != tt.want {
				t.Errorf("ToolCalls = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCodexEventToStreamEvent_ToolCalls(t *testing.T) {
	e := CodexEvent{Type: "tool_use", Message: &ClaudeMessage{
		ToolUses: []ToolUseBlock{{Name: "shell", Input: map[string]any{"command": "ls"}}},
	}}
	if got := codexEventToStreamEvent(e).ToolCalls; got != 1 {
		t.Errorf("ToolCalls = %d, want 1", got)
	}
}

func TestParseCodexItemLineTyped_NonBashTools(t *testing.T) {
	// Real Codex item types beyond command_execution must count as tool calls.
	for _, tt := range []struct{ itemType, wantName string }{
		{"file_change", "Edit"},
		{"collab_tool_call", "MCP"},
		{"web_search", "WebSearch"},
	} {
		t.Run(tt.itemType, func(t *testing.T) {
			ev, err := parseCodexItemLineTyped("item.started", &codexItem{Type: tt.itemType, ID: "x"}, nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if ev.Type != "tool_use" || len(ev.Message.ToolUses) != 1 || ev.Message.ToolUses[0].Name != tt.wantName {
				t.Fatalf("started: got type=%q name=%v, want tool_use/%s", ev.Type, ev.Message, tt.wantName)
			}
			if got := codexEventToStreamEvent(ev).ToolCalls; got != 1 {
				t.Errorf("ToolCalls = %d, want 1", got)
			}
			// Completed half must not double-count.
			done, _ := parseCodexItemLineTyped("item.completed", &codexItem{Type: tt.itemType, ID: "x"}, nil)
			if codexEventToStreamEvent(done).ToolCalls != 0 {
				t.Errorf("completed counted a tool call (double count)")
			}
		})
	}
}
