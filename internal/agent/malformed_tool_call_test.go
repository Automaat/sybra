package agent

import (
	"io"
	"strings"
	"testing"
	"time"
)

type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

func TestClassifyMalformedToolCall(t *testing.T) {
	t.Parallel()

	tr := ToolResultBlock{
		ToolUseID: "toolu_1",
		IsError:   true,
		Content: "Validation error: missing required property \"cmd\".\n" +
			"Expected schema: {\"type\":\"object\",\"required\":[\"cmd\"]}",
	}
	got, ok := classifyMalformedToolCall(tr)
	if !ok {
		t.Fatal("classifyMalformedToolCall = false, want true")
	}
	if !strings.Contains(got.ValidationError, "missing required property") {
		t.Fatalf("ValidationError = %q, want validation text", got.ValidationError)
	}
	if !strings.Contains(got.ExpectedSchema, "\"required\":[\"cmd\"]") {
		t.Fatalf("ExpectedSchema = %q, want schema text", got.ExpectedSchema)
	}
}

func TestProcessHeadlessLine_QueuesMalformedToolCorrection(t *testing.T) {
	t.Parallel()

	m := newParseTestManager(t)
	a := &Agent{
		ID:        "malformed-correct",
		TaskID:    "task-1",
		Mode:      "headless",
		Provider:  "claude",
		StartedAt: time.Now().UTC(),
	}
	a.convo.replaceStdinPipe(discardWriteCloser{})

	lastEmit := time.Now().Add(-time.Minute)
	prov := providerByName("claude")
	toolUse := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"functions.exec_command","input":{"cmd":"pwd"}}]}}`)
	if stop := m.processHeadlessLine(t.Context(), a, toolUse, &lastEmit, prov); stop {
		t.Fatal("tool_use event must not stop the stream")
	}
	toolResult := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"Validation error: missing required property \"cmd\".\nExpected schema: {\"type\":\"object\",\"required\":[\"cmd\"]}"}]}}`)
	if stop := m.processHeadlessLine(t.Context(), a, toolResult, &lastEmit, prov); stop {
		t.Fatal("tool_result event must not stop the stream")
	}

	if got := a.PendingPromptCount(); got != 1 {
		t.Fatalf("PendingPromptCount = %d, want 1", got)
	}
	if got := a.GetMalformedToolCorrectionAttempts(); got != 1 {
		t.Fatalf("GetMalformedToolCorrectionAttempts = %d, want 1", got)
	}
	if got := a.GetErrorKind(); got != "" {
		t.Fatalf("GetErrorKind = %q, want empty after in-session correction", got)
	}
	records := a.GetMalformedToolCalls()
	if len(records) != 1 {
		t.Fatalf("GetMalformedToolCalls len = %d, want 1", len(records))
	}
	if records[0].Outcome != malformedToolCallOutcomeCorrected {
		t.Fatalf("Outcome = %q, want %q", records[0].Outcome, malformedToolCallOutcomeCorrected)
	}
	if records[0].Tool != "functions.exec_command" {
		t.Fatalf("Tool = %q, want functions.exec_command", records[0].Tool)
	}

	out := a.Output()
	if len(out) == 0 {
		t.Fatal("expected user_input event in output")
	}
	last := out[len(out)-1]
	if last.Type != "user_input" {
		t.Fatalf("last output type = %q, want user_input", last.Type)
	}
	if !strings.Contains(last.Content, "Tool-call correction only.") ||
		!strings.Contains(last.Content, "Previous input:") {
		t.Fatalf("user_input content = %q, want correction prompt", last.Content)
	}
}

func TestProcessHeadlessLine_RepeatedMalformedToolCallTriggersFallback(t *testing.T) {
	t.Parallel()

	m := newParseTestManager(t)
	gate := &fakeGate{healthy: map[string]bool{"claude": true, "codex": true}}
	m.SetHealthGate(gate)
	a := &Agent{
		ID:        "malformed-fallback",
		TaskID:    "task-2",
		Mode:      "headless",
		Provider:  "claude",
		StartedAt: time.Now().UTC(),
	}
	a.convo.replaceStdinPipe(discardWriteCloser{})
	a.IncMalformedToolCorrectionAttempts()

	lastEmit := time.Now().Add(-time.Minute)
	prov := providerByName("claude")
	toolUse := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"pwd"}}]}}`)
	if stop := m.processHeadlessLine(t.Context(), a, toolUse, &lastEmit, prov); stop {
		t.Fatal("tool_use event must not stop the stream")
	}
	toolResult := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":"Validation error: missing required property \"command\".\nExpected schema: {\"type\":\"object\",\"required\":[\"command\"]}"}]}}`)
	if stop := m.processHeadlessLine(t.Context(), a, toolResult, &lastEmit, prov); stop {
		t.Fatal("tool_result event must not stop the stream")
	}

	if got := a.GetErrorKind(); got != malformedToolCallErrorKind {
		t.Fatalf("GetErrorKind = %q, want %q", got, malformedToolCallErrorKind)
	}
	if got := a.PendingPromptCount(); got != 0 {
		t.Fatalf("PendingPromptCount = %d, want 0 after unrecoverable malformed call", got)
	}
	records := a.GetMalformedToolCalls()
	if len(records) != 1 {
		t.Fatalf("GetMalformedToolCalls len = %d, want 1", len(records))
	}
	if records[0].Outcome != malformedToolCallOutcomeUnrecoverable {
		t.Fatalf("Outcome = %q, want %q", records[0].Outcome, malformedToolCallOutcomeUnrecoverable)
	}
	if len(gate.reportedRLName) != 1 || gate.reportedRLName[0] != "claude" {
		t.Fatalf("reportedRLName = %v, want [claude]", gate.reportedRLName)
	}
	if len(gate.reportedRLDelay) != 1 || gate.reportedRLDelay[0] != malformedToolFailoverCooldown {
		t.Fatalf("reportedRLDelay = %v, want [%s]", gate.reportedRLDelay, malformedToolFailoverCooldown)
	}
}

var _ io.WriteCloser = discardWriteCloser{}
