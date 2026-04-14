package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// makeAssistantLine builds one realistic NDJSON assistant line used by the
// benchmarks to avoid timing the builder overhead.
func makeAssistantLine(i int) []byte {
	event := map[string]any{
		"type":       "assistant",
		"session_id": "bench-session",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": fmt.Sprintf("bench text line %d with a reasonable amount of body so allocs look realistic", i),
				},
				map[string]any{
					"type": "tool_use",
					"id":   fmt.Sprintf("tu_%d", i),
					"name": "Bash",
					"input": map[string]any{
						"command":     fmt.Sprintf("echo bench %d", i),
						"description": "run a small echo",
					},
				},
			},
		},
	}
	b, _ := json.Marshal(event)
	return b
}

// makeResultLine builds a realistic result event.
func makeResultLine() []byte {
	event := map[string]any{
		"type":                "result",
		"session_id":          "bench-session",
		"result":              "Task completed successfully.",
		"total_cost_usd":      0.12,
		"total_input_tokens":  1234.0,
		"total_output_tokens": 567.0,
	}
	b, _ := json.Marshal(event)
	return b
}

// BenchmarkParseClaudeLine_Assistant measures per-line parse cost for the
// most common event type. This runs on every stream line (50ms throttle only
// caps emit, not parse) so regressions here scale linearly with event volume.
func BenchmarkParseClaudeLine_Assistant(b *testing.B) {
	line := makeAssistantLine(0)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		if _, err := ParseClaudeLine(line); err != nil {
			b.Fatalf("ParseClaudeLine: %v", err)
		}
	}
}

// BenchmarkParseClaudeLine_Result measures the terminal-event hot path.
func BenchmarkParseClaudeLine_Result(b *testing.B) {
	line := makeResultLine()
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		if _, err := ParseClaudeLine(line); err != nil {
			b.Fatalf("ParseClaudeLine: %v", err)
		}
	}
}

// BenchmarkParseClaudeLine_Mixed simulates a realistic mix of events (9
// assistant per 1 result) by rotating through a precomputed slice. Models
// the steady-state streaming workload.
func BenchmarkParseClaudeLine_Mixed(b *testing.B) {
	lines := make([][]byte, 10)
	for i := range 9 {
		lines[i] = makeAssistantLine(i)
	}
	lines[9] = makeResultLine()
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		if _, err := ParseClaudeLine(lines[i%len(lines)]); err != nil {
			b.Fatalf("ParseClaudeLine: %v", err)
		}
	}
}

// BenchmarkClaudeToStreamEvent measures conversion cost from parsed
// ClaudeEvent to the lightweight StreamEvent pushed to the frontend. Isolated
// from parse to identify whether regressions live in parsing or conversion.
func BenchmarkClaudeToStreamEvent(b *testing.B) {
	ce, err := ParseClaudeLine(makeAssistantLine(0))
	if err != nil {
		b.Fatalf("seed parse: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_ = claudeEventToStreamEvent(ce)
	}
}

// BenchmarkStreamEventMarshal measures the JSON encoding cost applied by
// sse.Broker.Emit on every outbound event. Every subscriber fanout reuses the
// same encoded payload so this runs once per event, not per subscriber.
func BenchmarkStreamEventMarshal(b *testing.B) {
	ev := StreamEvent{
		Type:         "assistant",
		Content:      "bench content with moderate length so the encoded bytes resemble real traffic",
		SessionID:    "bench-session",
		InputTokens:  123,
		OutputTokens: 45,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		buf.Reset()
		if err := enc.Encode(ev); err != nil {
			b.Fatalf("encode: %v", err)
		}
	}
}
