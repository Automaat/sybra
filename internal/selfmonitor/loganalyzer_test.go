package selfmonitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/synapse/internal/agent"
)

// fixtureLines returns a hand-written NDJSON stream matching the Claude
// stream-json envelope written by internal/agent/runner_headless.go:314.
// Three Bash calls use the same input so the repeated-call and stall
// detectors should both fire; the final result event reports a rate_limit
// error so classifyResultError exercises its structured-error path.
func fixtureLines() []string {
	return []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[` +
			`{"type":"text","text":"I will list the files"},` +
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls /tmp"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"t1","content":"file1\nfile2"}` +
			`]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[` +
			`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls /tmp"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"t2","content":"permission denied: /tmp/secret","is_error":true}` +
			`]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[` +
			`{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls /tmp"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"t3","content":"file1\nfile2"}` +
			`]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[` +
			`{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"ls /tmp"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"t4","content":"file1\nfile2"}` +
			`]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[` +
			`{"type":"tool_use","id":"t5","name":"Bash","input":{"command":"ls /tmp"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"t5","content":"file1\nfile2"}` +
			`]}}`,
		`{"type":"result","subtype":"error","session_id":"s1","total_cost_usd":0.42,` +
			`"total_input_tokens":1200,"total_output_tokens":340}`,
	}
}

func writeFixture(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-abc.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestAnalyzeToolLoopFixture(t *testing.T) {
	path := writeFixture(t, fixtureLines())

	s, err := Analyze(path, 0)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if s.SchemaVersion != LogSummarySchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", s.SchemaVersion, LogSummarySchemaVersion)
	}
	if s.TotalToolCalls != 5 {
		t.Errorf("TotalToolCalls = %d, want 5", s.TotalToolCalls)
	}
	if s.ToolHistogram["Bash"] != 5 {
		t.Errorf("ToolHistogram[Bash] = %d, want 5", s.ToolHistogram["Bash"])
	}
	if s.TotalCostUSD != 0.42 {
		t.Errorf("TotalCostUSD = %.4f, want 0.42", s.TotalCostUSD)
	}
	if s.TotalInputTokens != 1200 || s.TotalOutputTokens != 340 {
		t.Errorf("tokens = in:%d out:%d, want 1200/340", s.TotalInputTokens, s.TotalOutputTokens)
	}

	if len(s.RepeatedCalls) != 1 {
		t.Fatalf("RepeatedCalls = %d, want 1", len(s.RepeatedCalls))
	}
	rc := s.RepeatedCalls[0]
	if rc.Tool != "Bash" || rc.Count != 5 {
		t.Errorf("RepeatedCall = %+v, want Bash x5", rc)
	}

	if !s.StallDetected {
		t.Error("StallDetected = false, want true after 5 identical Bash calls")
	}
	if !strings.Contains(s.StallReason, "Bash") {
		t.Errorf("StallReason = %q, should mention Bash", s.StallReason)
	}

	// InferredCostPerTool should allocate the entire cost to Bash (only tool
	// in the stream).
	if got := s.InferredCostPerTool["Bash"]; got < 0.41 || got > 0.43 {
		t.Errorf("InferredCostPerTool[Bash] = %.4f, want ~0.42", got)
	}

	// Error classes: one permission_denied from the tool_result.
	foundPerm := false
	for _, ec := range s.ErrorClasses {
		if ec.Class == "permission_denied" && ec.Count == 1 {
			foundPerm = true
			break
		}
	}
	if !foundPerm {
		t.Errorf("ErrorClasses missing permission_denied: %+v", s.ErrorClasses)
	}

	if s.Path != path {
		t.Errorf("Path = %q, want %q", s.Path, path)
	}

	if len(s.LastToolCalls) == 0 || len(s.LastToolCalls) > DefaultLastToolCalls {
		t.Errorf("LastToolCalls = %d, want 1..%d", len(s.LastToolCalls), DefaultLastToolCalls)
	}
}

func TestAnalyzeEmptyLog(t *testing.T) {
	path := writeFixture(t, []string{})

	s, err := Analyze(path, 0)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if s.TotalEvents != 0 || s.TotalToolCalls != 0 {
		t.Errorf("empty log: %+v", s)
	}
	if s.StallDetected {
		t.Error("empty log should not flag stall")
	}
}

func TestAggregateRepeatedCallsDirect(t *testing.T) {
	// Construct events directly to bypass the NDJSON parser — faster and
	// avoids coupling this test to the stream-json envelope format.
	mkCall := func(id, name, cmd string) agent.ClaudeEvent {
		return agent.ClaudeEvent{
			Type: "assistant",
			Message: &agent.ClaudeMessage{
				Role: "assistant",
				ToolUses: []agent.ToolUseBlock{{
					ID:    id,
					Name:  name,
					Input: map[string]any{"command": cmd},
				}},
			},
		}
	}
	events := []agent.ClaudeEvent{
		mkCall("1", "Bash", "echo hi"),
		mkCall("2", "Bash", "echo hi"),
		mkCall("3", "Bash", "echo hi"),
		mkCall("4", "Read", "/file"),
	}
	s := aggregate(events)

	if s.ToolHistogram["Bash"] != 3 {
		t.Errorf("Bash count = %d, want 3", s.ToolHistogram["Bash"])
	}
	if len(s.RepeatedCalls) != 1 {
		t.Fatalf("RepeatedCalls = %d, want 1", len(s.RepeatedCalls))
	}
	if s.RepeatedCalls[0].Tool != "Bash" || s.RepeatedCalls[0].Count != 3 {
		t.Errorf("RepeatedCalls[0] = %+v", s.RepeatedCalls[0])
	}
}

func TestAggregateCostAttributionUsesToolUseID(t *testing.T) {
	events := []agent.ClaudeEvent{
		{
			Type: "assistant",
			Message: &agent.ClaudeMessage{
				Role: "assistant",
				ToolUses: []agent.ToolUseBlock{
					{ID: "a", Name: "Read", Input: map[string]any{"path": "/tmp/a"}},
					{ID: "b", Name: "Bash", Input: map[string]any{"command": "echo hi"}},
				},
			},
		},
		{
			Type: "user",
			Message: &agent.ClaudeMessage{
				Role: "user",
				ToolResults: []agent.ToolResultBlock{
					{ToolUseID: "a", Content: strings.Repeat("A", 1200)},
					{ToolUseID: "b", Content: strings.Repeat("b", 10)},
				},
			},
		},
		{
			Type: "result",
			Result: &agent.ClaudeResult{
				CostUSD: 1.0,
			},
		},
	}

	s := aggregate(events)
	if s.TotalToolCalls != 2 {
		t.Fatalf("TotalToolCalls = %d, want 2", s.TotalToolCalls)
	}
	readCost := s.InferredCostPerTool["Read"]
	bashCost := s.InferredCostPerTool["Bash"]
	if readCost <= bashCost {
		t.Fatalf("expected Read cost > Bash cost, got Read=%.4f Bash=%.4f", readCost, bashCost)
	}
	if readCost < 0.90 {
		t.Fatalf("Read cost too low: %.4f, want > 0.90", readCost)
	}
	if bashCost > 0.10 {
		t.Fatalf("Bash cost too high: %.4f, want < 0.10", bashCost)
	}
}

func TestClassifyResultError(t *testing.T) {
	tests := []struct {
		errType string
		status  int
		text    string
		want    string
	}{
		{"rate_limit_error", 429, "", "rate_limit"},
		{"overloaded_error", 529, "", "overloaded_error"},
		{"", 401, "", "auth_error"},
		{"", 0, "connection refused", "network"},
		{"", 0, "permission denied", "permission_denied"},
		{"", 0, "", "unknown"},
	}
	for _, tt := range tests {
		got := classifyResultError(tt.errType, tt.status, tt.text)
		if got != tt.want {
			t.Errorf("classifyResultError(%q,%d,%q) = %q, want %q",
				tt.errType, tt.status, tt.text, got, tt.want)
		}
	}
}
