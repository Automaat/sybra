package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStrVal(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
		key  string
		want string
	}{
		{"existing key", map[string]any{"foo": "bar"}, "foo", "bar"},
		{"missing key", map[string]any{"foo": "bar"}, "baz", ""},
		{"non-string value", map[string]any{"num": 42}, "num", ""},
		{"nil map value", map[string]any{"k": nil}, "k", ""},
		{"empty string", map[string]any{"k": ""}, "k", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strVal(tt.m, tt.key)
			if got != tt.want {
				t.Errorf("strVal() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseClaudeLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr bool
		check   func(t *testing.T, got ClaudeEvent)
	}{
		{
			name:    "invalid json",
			line:    "not json",
			wantErr: true,
		},
		{
			name: "empty object",
			line: `{}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "" {
					t.Errorf("Type = %q, want empty", got.Type)
				}
			},
		},
		{
			name: "system event with session_id",
			line: `{"type":"system","session_id":"sess-123","subtype":"init"}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "system" {
					t.Errorf("Type = %q, want system", got.Type)
				}
				if got.SessionID != "sess-123" {
					t.Errorf("SessionID = %q, want sess-123", got.SessionID)
				}
				if got.Subtype != "init" {
					t.Errorf("Subtype = %q, want init", got.Subtype)
				}
			},
		},
		{
			name: "result event",
			line: `{"type":"result","result":"done","session_id":"sess-456","total_cost_usd":0.05,"total_input_tokens":100,"total_output_tokens":50}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "result" {
					t.Errorf("Type = %q, want result", got.Type)
				}
				if got.Result == nil {
					t.Fatal("Result is nil")
				}
				if got.Result.Text != "done" {
					t.Errorf("Result.Text = %q, want done", got.Result.Text)
				}
				if got.Result.SessionID != "sess-456" {
					t.Errorf("Result.SessionID = %q, want sess-456", got.Result.SessionID)
				}
				if got.Result.CostUSD != 0.05 {
					t.Errorf("Result.CostUSD = %f, want 0.05", got.Result.CostUSD)
				}
				if got.Result.InputTokens != 100 {
					t.Errorf("Result.InputTokens = %d, want 100", got.Result.InputTokens)
				}
				if got.Result.OutputTokens != 50 {
					t.Errorf("Result.OutputTokens = %d, want 50", got.Result.OutputTokens)
				}
			},
		},
		{
			name: "result event without cost",
			line: `{"type":"result","result":"ok","session_id":"s1"}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Result == nil {
					t.Fatal("Result is nil")
				}
				if got.Result.CostUSD != 0 {
					t.Errorf("CostUSD = %f, want 0", got.Result.CostUSD)
				}
			},
		},
		{
			name: "unknown event type preserved",
			line: `{"type":"rate_limit_event","subtype":"throttle"}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "rate_limit_event" {
					t.Errorf("Type = %q, want rate_limit_event", got.Type)
				}
				if got.Subtype != "throttle" {
					t.Errorf("Subtype = %q, want throttle", got.Subtype)
				}
			},
		},
		{
			name: "assistant event with text content",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "assistant" {
					t.Errorf("Type = %q, want assistant", got.Type)
				}
				if got.Message == nil {
					t.Fatal("Message is nil")
				}
				if got.Message.Text != "hello" {
					t.Errorf("Message.Text = %q, want hello", got.Message.Text)
				}
			},
		},
		{
			name: "user event with tool result",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"result text"}]}}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if got.Type != "user" {
					t.Errorf("Type = %q, want user", got.Type)
				}
				if got.Message == nil {
					t.Fatal("Message is nil")
				}
				if len(got.Message.ToolResults) != 1 {
					t.Fatalf("len(ToolResults) = %d, want 1", len(got.Message.ToolResults))
				}
				if got.Message.ToolResults[0].Content != "result text" {
					t.Errorf("ToolResults[0].Content = %q, want result text", got.Message.ToolResults[0].Content)
				}
			},
		},
		{
			name: "raw is populated",
			line: `{"type":"system","session_id":"s1"}`,
			check: func(t *testing.T, got ClaudeEvent) {
				if len(got.Raw) == 0 {
					t.Error("Raw is empty")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseClaudeLine([]byte(tt.line))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestExtractAssistantContent(t *testing.T) {
	tests := []struct {
		name      string
		msg       map[string]any
		wantText  string
		wantTools int
	}{
		{
			name:     "no content key",
			msg:      map[string]any{},
			wantText: "",
		},
		{
			name:     "content not a slice",
			msg:      map[string]any{"content": "text"},
			wantText: "",
		},
		{
			name: "single text block",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "hello world"},
				},
			},
			wantText: "hello world",
		},
		{
			name: "multiple text blocks",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "line1"},
					map[string]any{"type": "text", "text": "line2"},
				},
			},
			wantText: "line1\nline2",
		},
		{
			name: "tool_use block",
			msg: map[string]any{
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "tu-1",
						"name":  "Bash",
						"input": map[string]any{"command": "ls -la"},
					},
				},
			},
			wantText:  "",
			wantTools: 1,
		},
		{
			name: "mixed text and tool_use",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "thinking..."},
					map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "pwd"}},
				},
			},
			wantText:  "thinking...",
			wantTools: 1,
		},
		{
			name: "non-map content block skipped",
			msg: map[string]any{
				"content": []any{
					"not a map",
					map[string]any{"type": "text", "text": "ok"},
				},
			},
			wantText: "ok",
		},
		{
			name: "unknown block type skipped",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "image", "data": "abc"},
				},
			},
			wantText: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAssistantContent(tt.msg)
			if got.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tt.wantText)
			}
			if len(got.ToolUses) != tt.wantTools {
				t.Errorf("len(ToolUses) = %d, want %d", len(got.ToolUses), tt.wantTools)
			}
		})
	}
}

func TestExtractAssistantContent_ToolUseFields(t *testing.T) {
	msg := map[string]any{
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tu-abc",
				"name":  "Read",
				"input": map[string]any{"description": "read a file"},
			},
		},
	}
	got := extractAssistantContent(msg)
	if len(got.ToolUses) != 1 {
		t.Fatalf("len(ToolUses) = %d, want 1", len(got.ToolUses))
	}
	tu := got.ToolUses[0]
	if tu.ID != "tu-abc" {
		t.Errorf("ID = %q, want tu-abc", tu.ID)
	}
	if tu.Name != "Read" {
		t.Errorf("Name = %q, want Read", tu.Name)
	}
	if desc, _ := tu.Input["description"].(string); desc != "read a file" {
		t.Errorf("Input[description] = %q, want read a file", desc)
	}
}

func TestExtractToolResults(t *testing.T) {
	tests := []struct {
		name    string
		msg     map[string]any
		wantLen int
		check   func(t *testing.T, results []ToolResultBlock)
	}{
		{
			name:    "no content key",
			msg:     map[string]any{},
			wantLen: 0,
		},
		{
			name:    "content not a slice",
			msg:     map[string]any{"content": true},
			wantLen: 0,
		},
		{
			name: "single tool_result block",
			msg: map[string]any{
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu-1",
						"content":     "tool output",
					},
				},
			},
			wantLen: 1,
			check: func(t *testing.T, results []ToolResultBlock) {
				if results[0].Content != "tool output" {
					t.Errorf("Content = %q, want tool output", results[0].Content)
				}
				if results[0].ToolUseID != "tu-1" {
					t.Errorf("ToolUseID = %q, want tu-1", results[0].ToolUseID)
				}
			},
		},
		{
			name: "block without type skipped",
			msg: map[string]any{
				"content": []any{
					map[string]any{"content": "invisible"},
				},
			},
			wantLen: 0,
		},
		{
			name: "non-tool_result type skipped",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "skipped"},
					map[string]any{"type": "tool_result", "content": "visible"},
				},
			},
			wantLen: 1,
		},
		{
			name: "is_error propagated",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "tool_result", "content": "err", "is_error": true},
				},
			},
			wantLen: 1,
			check: func(t *testing.T, results []ToolResultBlock) {
				if !results[0].IsError {
					t.Error("IsError = false, want true")
				}
			},
		},
		{
			name: "array content joined",
			msg: map[string]any{
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": []any{
							map[string]any{"text": "part1"},
							map[string]any{"text": "part2"},
						},
					},
				},
			},
			wantLen: 1,
			check: func(t *testing.T, results []ToolResultBlock) {
				if results[0].Content != "part1\npart2" {
					t.Errorf("Content = %q, want part1\\npart2", results[0].Content)
				}
			},
		},
		{
			name: "content not truncated by shared parser",
			msg: map[string]any{
				"content": []any{
					map[string]any{"type": "tool_result", "content": strings.Repeat("x", 3000)},
				},
			},
			wantLen: 1,
			check: func(t *testing.T, results []ToolResultBlock) {
				if len(results[0].Content) != 3000 {
					t.Errorf("len(Content) = %d, want 3000 (no truncation in shared parser)", len(results[0].Content))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractToolResults(tt.msg)
			if len(got) != tt.wantLen {
				t.Fatalf("len(results) = %d, want %d", len(got), tt.wantLen)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// TestParseClaudeLine_RawIsIndependentOfScannerBuffer locks in the fix for a
// JSON marshal crash that occurs when Raw aliases the scanner's internal buffer.
func TestParseClaudeLine_RawIsIndependentOfScannerBuffer(t *testing.T) {
	mkLine := func(id string, fill int) string {
		return `{"type":"assistant","session_id":"` + id +
			`","payload":"` + strings.Repeat("x", fill) + `"}`
	}
	line1 := mkLine("s-one", 256)
	line2 := mkLine("s-two", 256)
	line3 := mkLine("s-three", 256)

	input := line1 + "\n" + line2 + "\n" + line3 + "\n"

	scanner := bufio.NewScanner(strings.NewReader(input))
	// Deliberately smaller than one line to force repeated refills.
	scanner.Buffer(make([]byte, 0, 64), 4096)

	var events []ClaudeEvent
	for scanner.Scan() {
		ev, err := ParseClaudeLine(scanner.Bytes())
		if err != nil {
			t.Fatalf("ParseClaudeLine: %v", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}

	// Each Raw must marshal cleanly and equal the original line — this is the
	// exact path that broke in production when Raw aliased the scanner buffer.
	for i, want := range []string{line1, line2, line3} {
		got, err := json.Marshal(events[i].Raw)
		if err != nil {
			t.Fatalf("marshal event[%d].Raw: %v", i, err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("event[%d].Raw = %s\nwant %s", i, got, want)
		}
	}
}

func TestParseCodexLine_AgentMessage(t *testing.T) {
	line := []byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}`)

	got, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if got.Type != "assistant" {
		t.Fatalf("Type = %q, want assistant", got.Type)
	}
	if got.Message == nil {
		t.Fatal("Message is nil")
	}
	if got.Message.Text != "hi" {
		t.Fatalf("Message.Text = %q, want hi", got.Message.Text)
	}
}

func TestParseCodexLine_CommandExecution(t *testing.T) {
	started := []byte(`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"","exit_code":null,"status":"in_progress"}}`)
	completed := []byte(`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"/repo\n","exit_code":0,"status":"completed"}}`)

	startEv, err := ParseCodexLine(started)
	if err != nil {
		t.Fatalf("ParseCodexLine started: %v", err)
	}
	if startEv.Type != "tool_use" {
		t.Fatalf("started Type = %q, want tool_use", startEv.Type)
	}
	if startEv.Message == nil || len(startEv.Message.ToolUses) == 0 {
		t.Fatal("started: no ToolUses")
	}
	if cmd, _ := startEv.Message.ToolUses[0].Input["command"].(string); cmd != "pwd" {
		t.Fatalf("started command = %q, want pwd", cmd)
	}

	doneEv, err := ParseCodexLine(completed)
	if err != nil {
		t.Fatalf("ParseCodexLine completed: %v", err)
	}
	if doneEv.Type != "tool_result" {
		t.Fatalf("completed Type = %q, want tool_result", doneEv.Type)
	}
	if doneEv.Message == nil || len(doneEv.Message.ToolResults) == 0 {
		t.Fatal("completed: no ToolResults")
	}
	if doneEv.Message.ToolResults[0].Content != "/repo\n" {
		t.Fatalf("completed Content = %q, want /repo\\n", doneEv.Message.ToolResults[0].Content)
	}
}

func TestParseCodexLine_TurnCompleted(t *testing.T) {
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":16012,"cached_input_tokens":2432,"output_tokens":18}}`)

	got, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if got.Type != "result" {
		t.Fatalf("Type = %q, want result", got.Type)
	}
	if got.Result == nil {
		t.Fatal("Result is nil")
	}
	if got.Result.InputTokens != 16012 || got.Result.OutputTokens != 18 {
		t.Fatalf("tokens = %d/%d, want 16012/18", got.Result.InputTokens, got.Result.OutputTokens)
	}
}

func TestParseCodexLine_ThreadStarted(t *testing.T) {
	line := []byte(`{"type":"thread.started","thread_id":"thread-abc"}`)

	got, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if got.Type != "init" {
		t.Fatalf("Type = %q, want init", got.Type)
	}
	if got.SessionID != "thread-abc" {
		t.Fatalf("SessionID = %q, want thread-abc", got.SessionID)
	}
}

// TestClaudeEventToStreamEvent verifies that the headless adapter produces
// output equivalent to the former parseClaudeStreamEvent implementation.
func TestClaudeEventToStreamEvent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want StreamEvent
	}{
		{
			name: "system event",
			line: `{"type":"system","session_id":"sess-123","subtype":"init"}`,
			want: StreamEvent{Type: "system", SessionID: "sess-123", Subtype: "init"},
		},
		{
			name: "result event",
			line: `{"type":"result","result":"done","session_id":"sess-456","total_cost_usd":0.05}`,
			want: StreamEvent{Type: "result", Content: "done", SessionID: "sess-456", CostUSD: 0.05},
		},
		{
			name: "unknown type preserved",
			line: `{"type":"rate_limit_event","subtype":"throttle"}`,
			want: StreamEvent{Type: "rate_limit_event", Subtype: "throttle"},
		},
		{
			name: "assistant text content",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
			want: StreamEvent{Type: "assistant", Content: "hello"},
		},
		{
			name: "assistant tool_use with command",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}`,
			want: StreamEvent{Type: "assistant", Content: "[Bash] ls -la"},
		},
		{
			name: "assistant tool_use with description",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"description":"read a file"}}]}}`,
			want: StreamEvent{Type: "assistant", Content: "[Read] read a file"},
		},
		{
			name: "assistant tool_use no desc or cmd",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Unknown","input":{}}]}}`,
			want: StreamEvent{Type: "assistant", Content: "[Unknown]"},
		},
		{
			name: "user tool result truncated at 500",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + strings.Repeat("x", 600) + `"}]}}`,
			want: StreamEvent{Type: "user", Content: strings.Repeat("x", 500) + "..."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseClaudeLine([]byte(tt.line))
			if err != nil {
				t.Fatalf("ParseClaudeLine: %v", err)
			}
			got := claudeEventToStreamEvent(parsed)
			if got.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", got.Type, tt.want.Type)
			}
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.SessionID != tt.want.SessionID {
				t.Errorf("SessionID = %q, want %q", got.SessionID, tt.want.SessionID)
			}
			if got.CostUSD != tt.want.CostUSD {
				t.Errorf("CostUSD = %f, want %f", got.CostUSD, tt.want.CostUSD)
			}
			if got.Subtype != tt.want.Subtype {
				t.Errorf("Subtype = %q, want %q", got.Subtype, tt.want.Subtype)
			}
		})
	}
}

// TestCodexEventToStreamEvent verifies the headless Codex adapter.
func TestCodexEventToStreamEvent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want StreamEvent
	}{
		{
			name: "agent_message",
			line: `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}`,
			want: StreamEvent{Type: "assistant", Content: "hi"},
		},
		{
			name: "command_execution started",
			line: `{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"pwd"}}`,
			want: StreamEvent{Type: "tool_use", Content: "pwd"},
		},
		{
			name: "command_execution completed",
			line: `{"type":"item.completed","item":{"id":"item_1","type":"command_execution","aggregated_output":"/repo\n","exit_code":0}}`,
			want: StreamEvent{Type: "tool_result", Content: "/repo\n"},
		},
		{
			name: "turn.completed tokens",
			line: `{"type":"turn.completed","usage":{"input_tokens":16012,"output_tokens":18}}`,
			want: StreamEvent{Type: "result", InputTokens: 16012, OutputTokens: 18},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseCodexLine([]byte(tt.line))
			if err != nil {
				t.Fatalf("ParseCodexLine: %v", err)
			}
			got := codexEventToStreamEvent(parsed)
			if got.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", got.Type, tt.want.Type)
			}
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.InputTokens != tt.want.InputTokens {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tt.want.InputTokens)
			}
			if got.OutputTokens != tt.want.OutputTokens {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tt.want.OutputTokens)
			}
		})
	}
}
