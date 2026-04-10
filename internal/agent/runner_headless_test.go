package agent

import "testing"

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
