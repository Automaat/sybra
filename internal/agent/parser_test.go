package agent

import (
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
			got := strVal(tt.m, tt.key)
			if got != tt.want {
				t.Errorf("strVal() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseStreamEvent(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    StreamEvent
		checkFn func(t *testing.T, got StreamEvent)
	}{
		{
			name: "invalid json",
			line: "not json",
			want: StreamEvent{},
		},
		{
			name: "empty object",
			line: `{}`,
			want: StreamEvent{},
		},
		{
			name: "system event with session_id",
			line: `{"type":"system","session_id":"sess-123","subtype":"init"}`,
			want: StreamEvent{Type: "system", SessionID: "sess-123", Subtype: "init"},
		},
		{
			name: "result event",
			line: `{"type":"result","result":"done","session_id":"sess-456","total_cost_usd":0.05}`,
			want: StreamEvent{Type: "result", Content: "done", SessionID: "sess-456", CostUSD: 0.05},
		},
		{
			name: "result event without cost",
			line: `{"type":"result","result":"ok","session_id":"s1"}`,
			want: StreamEvent{Type: "result", Content: "ok", SessionID: "s1", CostUSD: 0},
		},
		{
			name: "unknown event type preserved",
			line: `{"type":"rate_limit_event","subtype":"throttle"}`,
			want: StreamEvent{Type: "rate_limit_event", Subtype: "throttle"},
		},
		{
			name: "assistant event with text content",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
			want: StreamEvent{Type: "assistant", Content: "hello"},
		},
		{
			name: "user event with tool result",
			line: `{"type":"user","message":{"content":[{"content":"result text"}]}}`,
			want: StreamEvent{Type: "user", Content: "result text"},
		},
		{
			name: "subtype extracted",
			line: `{"type":"system","subtype":"greeting"}`,
			want: StreamEvent{Type: "system", Subtype: "greeting"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStreamEvent([]byte(tt.line))
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

func TestExtractMessageContent(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "no message key",
			raw:  map[string]any{"type": "assistant"},
			want: "",
		},
		{
			name: "message not a map",
			raw:  map[string]any{"message": "string"},
			want: "",
		},
		{
			name: "no content in message",
			raw:  map[string]any{"message": map[string]any{}},
			want: "",
		},
		{
			name: "content not a slice",
			raw:  map[string]any{"message": map[string]any{"content": "text"}},
			want: "",
		},
		{
			name: "single text block",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "hello world"},
					},
				},
			},
			want: "hello world",
		},
		{
			name: "multiple text blocks",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "line1"},
						map[string]any{"type": "text", "text": "line2"},
					},
				},
			},
			want: "line1\nline2",
		},
		{
			name: "tool_use with description",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"name":  "Read",
							"input": map[string]any{"description": "read a file"},
						},
					},
				},
			},
			want: "[Read] read a file",
		},
		{
			name: "tool_use with command",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"name":  "Bash",
							"input": map[string]any{"command": "ls -la"},
						},
					},
				},
			},
			want: "[Bash] ls -la",
		},
		{
			name: "tool_use with no desc or cmd",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"name":  "Unknown",
							"input": map[string]any{},
						},
					},
				},
			},
			want: "[Unknown]",
		},
		{
			name: "tool_use with nil input",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{
							"type": "tool_use",
							"name": "Tool",
						},
					},
				},
			},
			want: "[Tool]",
		},
		{
			name: "mixed text and tool_use",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "thinking..."},
						map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "pwd"}},
					},
				},
			},
			want: "thinking...\n[Bash] pwd",
		},
		{
			name: "non-map content block skipped",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						"not a map",
						map[string]any{"type": "text", "text": "ok"},
					},
				},
			},
			want: "ok",
		},
		{
			name: "unknown block type skipped",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "image", "data": "abc"},
					},
				},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMessageContent(tt.raw)
			if got != tt.want {
				t.Errorf("extractMessageContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractToolResult(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "no message key",
			raw:  map[string]any{"type": "user"},
			want: "",
		},
		{
			name: "message not a map",
			raw:  map[string]any{"message": 42},
			want: "",
		},
		{
			name: "no content in message",
			raw:  map[string]any{"message": map[string]any{}},
			want: "",
		},
		{
			name: "content not a slice",
			raw:  map[string]any{"message": map[string]any{"content": true}},
			want: "",
		},
		{
			name: "single result",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"content": "tool output"},
					},
				},
			},
			want: "tool output",
		},
		{
			name: "multiple results",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"content": "out1"},
						map[string]any{"content": "out2"},
					},
				},
			},
			want: "out1\nout2",
		},
		{
			name: "empty content string skipped",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"content": ""},
						map[string]any{"content": "visible"},
					},
				},
			},
			want: "visible",
		},
		{
			name: "non-string content skipped",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"content": 123},
						map[string]any{"content": "ok"},
					},
				},
			},
			want: "ok",
		},
		{
			name: "non-map block skipped",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						"not a map",
						map[string]any{"content": "found"},
					},
				},
			},
			want: "found",
		},
		{
			name: "long content truncated at 500",
			raw: map[string]any{
				"message": map[string]any{
					"content": []any{
						map[string]any{"content": strings.Repeat("x", 600)},
					},
				},
			},
			want: strings.Repeat("x", 500) + "...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractToolResult(tt.raw)
			if got != tt.want {
				t.Errorf("extractToolResult() = %q, want %q", got, tt.want)
			}
		})
	}
}
