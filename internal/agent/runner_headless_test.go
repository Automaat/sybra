package agent

import (
	"testing"
)

func TestParseCodexStreamEvent_AgentMessage(t *testing.T) {
	line := []byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "assistant" {
		t.Fatalf("Type = %q, want assistant", got.Type)
	}
	if got.Content != "hi" {
		t.Fatalf("Content = %q, want hi", got.Content)
	}
}

func TestParseCodexStreamEvent_CommandExecution(t *testing.T) {
	started := []byte(`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"","exit_code":null,"status":"in_progress"}}`)
	completed := []byte(`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"/repo\n","exit_code":0,"status":"completed"}}`)

	startParsed, err := ParseCodexLine(started)
	if err != nil {
		t.Fatalf("ParseCodexLine started: %v", err)
	}
	startEv := codexEventToStreamEvent(startParsed)
	if startEv.Type != "tool_use" || startEv.Content != "pwd" {
		t.Fatalf("started = %#v, want tool_use pwd", startEv)
	}

	doneParsed, err := ParseCodexLine(completed)
	if err != nil {
		t.Fatalf("ParseCodexLine completed: %v", err)
	}
	doneEv := codexEventToStreamEvent(doneParsed)
	if doneEv.Type != "tool_result" || doneEv.Content != "/repo\n" {
		t.Fatalf("completed = %#v, want tool_result output", doneEv)
	}
}

func TestParseCodexStreamEvent_TurnCompleted(t *testing.T) {
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":16012,"cached_input_tokens":2432,"output_tokens":18}}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "result" {
		t.Fatalf("Type = %q, want result", got.Type)
	}
	if got.InputTokens != 16012 || got.OutputTokens != 18 {
		t.Fatalf("tokens = %d/%d, want 16012/18", got.InputTokens, got.OutputTokens)
	}
}

func TestParseCodexStreamEvent_Error_SubstringFallback(t *testing.T) {
	line := []byte(`{"type":"error","message":"Service overloaded (529)"}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Type != "result" || parsed.Subtype != "error" {
		t.Fatalf("got type=%q subtype=%q, want result/error", parsed.Type, parsed.Subtype)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "result" || got.Subtype != "error" {
		t.Fatalf("StreamEvent type=%q subtype=%q, want result/error", got.Type, got.Subtype)
	}

	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (substring fallback on overloaded message)")
	}
}

func TestParseCodexStreamEvent_Error_StructuredCode(t *testing.T) {
	line := []byte(`{"type":"error","message":"Service overloaded","code":529}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Result == nil {
		t.Fatal("Result is nil")
	}
	if parsed.Result.ErrorStatus != 529 {
		t.Fatalf("ErrorStatus = %d, want 529", parsed.Result.ErrorStatus)
	}

	got := codexEventToStreamEvent(parsed)
	if got.ErrorStatus != 529 {
		t.Fatalf("StreamEvent.ErrorStatus = %d, want 529", got.ErrorStatus)
	}
	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (structured ErrorStatus=529)")
	}
}

func TestParseCodexStreamEvent_Error_StructuredErrorType(t *testing.T) {
	line := []byte(`{"type":"error","message":"Overloaded","error_type":"overloaded_error","code":529}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Result == nil {
		t.Fatal("Result is nil")
	}
	if parsed.Result.ErrorType != "overloaded_error" {
		t.Fatalf("ErrorType = %q, want overloaded_error", parsed.Result.ErrorType)
	}

	got := codexEventToStreamEvent(parsed)
	if got.ErrorType != "overloaded_error" {
		t.Fatalf("StreamEvent.ErrorType = %q, want overloaded_error", got.ErrorType)
	}
	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (structured ErrorType=overloaded_error)")
	}
}

func TestShouldRetry_Stderr529(t *testing.T) {
	if !shouldRetry("error: 529 overloaded", nil, nil) {
		t.Fatal("shouldRetry = false on stderr containing 529")
	}
}

func TestShouldRetry_FatalError_NoRetry(t *testing.T) {
	events := []StreamEvent{{Type: "result", Subtype: "error", Content: "permission denied"}}
	if shouldRetry("", events, nil) {
		t.Fatal("shouldRetry = true on non-transient error, want false")
	}
}
