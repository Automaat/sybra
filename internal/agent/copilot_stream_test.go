package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// These fixtures are real lines captured from `copilot --output-format json`
// (v1.0.63) running two headless prompts: a trivial reply and a shell tool use.

func TestParseCopilotLine_SkipsEphemeralAndStructural(t *testing.T) {
	lines := []string{
		// session status (ephemeral) — carries the model but no session id.
		`{"type":"session.tools_updated","data":{"model":"claude-sonnet-4.6"},"id":"a","ephemeral":true}`,
		`{"type":"session.mcp_servers_loaded","data":{"servers":[]},"id":"b","ephemeral":true}`,
		// streaming deltas (ephemeral).
		`{"type":"assistant.message_delta","data":{"deltaContent":"p"},"id":"c","ephemeral":true}`,
		`{"type":"assistant.reasoning_delta","data":{"deltaContent":"x"},"id":"d","ephemeral":true}`,
		`{"type":"tool.execution_partial_result","data":{"toolCallId":"t1","partialOutput":"hi"},"id":"e","ephemeral":true}`,
		// structural / echo (non-ephemeral but no displayable content).
		`{"type":"user.message","data":{"content":"hi"},"id":"f"}`,
		`{"type":"assistant.turn_start","data":{"turnId":"0"},"id":"g"}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"h"}`,
	}
	for _, line := range lines {
		ev, err := ParseCopilotLine([]byte(line))
		if err != nil {
			t.Fatalf("ParseCopilotLine(%s): %v", line, err)
		}
		if ev.Type != "" {
			t.Errorf("expected skipped (Type==\"\") for %s, got %q", line, ev.Type)
		}
	}
}

func TestParseCopilotLine_AssistantMessage(t *testing.T) {
	line := []byte(`{"type":"assistant.message","data":{"messageId":"m1","model":"claude-sonnet-4.6","content":"pong","toolRequests":[],"outputTokens":5},"id":"x"}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}
	if ev.Type != "assistant" {
		t.Fatalf("Type = %q, want assistant", ev.Type)
	}
	if ev.Message == nil || ev.Message.Text != "pong" {
		t.Fatalf("Message.Text = %v, want pong", ev.Message)
	}
	if ev.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", ev.OutputTokens)
	}
}

func TestParseCopilotLine_AssistantToolRequest(t *testing.T) {
	line := []byte(`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"toolu_1","name":"bash","arguments":{"command":"cat sample.txt","description":"read file"},"type":"function"}],"outputTokens":98},"id":"x"}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}
	if ev.Type != "assistant" || ev.Message == nil || len(ev.Message.ToolUses) != 1 {
		t.Fatalf("want one tool use, got %+v", ev.Message)
	}
	tu := ev.Message.ToolUses[0]
	if tu.ID != "toolu_1" {
		t.Errorf("tool id = %q, want toolu_1", tu.ID)
	}
	if tu.Name != "Bash" {
		t.Errorf("tool name = %q, want Bash (normalized from bash)", tu.Name)
	}
	if cmd, _ := tu.Input["command"].(string); cmd != "cat sample.txt" {
		t.Errorf("command = %q, want cat sample.txt", cmd)
	}
}

func TestParseCopilotLine_ToolExecution(t *testing.T) {
	start := []byte(`{"type":"tool.execution_start","data":{"toolCallId":"toolu_1","toolName":"bash","arguments":{"command":"cat sample.txt","description":"read file"}},"id":"s"}`)
	ev, err := ParseCopilotLine(start)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ev.Type != "tool_use" || ev.Message == nil || len(ev.Message.ToolUses) != 1 {
		t.Fatalf("start: want tool_use with one tool, got %+v", ev)
	}
	if ev.Message.ToolUses[0].Name != "Bash" {
		t.Errorf("start tool name = %q, want Bash", ev.Message.ToolUses[0].Name)
	}

	done := []byte(`{"type":"tool.execution_complete","data":{"toolCallId":"toolu_1","success":true,"result":{"content":"hello world\n<shellId: 0 completed with exit code 0>"}},"id":"c"}`)
	ev, err = ParseCopilotLine(done)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if ev.Type != "tool_result" || ev.Message == nil || len(ev.Message.ToolResults) != 1 {
		t.Fatalf("complete: want tool_result with one result, got %+v", ev)
	}
	tr := ev.Message.ToolResults[0]
	if tr.ToolUseID != "toolu_1" {
		t.Errorf("tool result id = %q, want toolu_1", tr.ToolUseID)
	}
	if tr.IsError {
		t.Errorf("IsError = true, want false (success:true)")
	}
	if tr.Content == "" {
		t.Errorf("tool result content empty")
	}
}

func TestParseCopilotLine_ToolExecutionFailure(t *testing.T) {
	done := []byte(`{"type":"tool.execution_complete","data":{"toolCallId":"t2","success":false,"result":{"content":"boom"}},"id":"c"}`)
	ev, err := ParseCopilotLine(done)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !ev.Message.ToolResults[0].IsError {
		t.Errorf("IsError = false, want true (success:false)")
	}
}

func TestParseCopilotLine_Result(t *testing.T) {
	line := []byte(`{"type":"result","sessionId":"bff77b8b-cc7e-4b96-b2ae-8f52dbff73ab","exitCode":0,"usage":{"premiumRequests":1,"sessionDurationMs":5700}}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}

	if ev.Type != "result" {
		t.Fatalf("Type = %q, want result", ev.Type)
	}
	if ev.SessionID != "bff77b8b-cc7e-4b96-b2ae-8f52dbff73ab" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.Result == nil || ev.Result.PremiumRequests != 1 {
		t.Errorf("PremiumRequests = %v, want 1", ev.Result)
	}
}

func TestParseCopilotLine_ResultFractionalPremiumRequests(t *testing.T) {
	line := []byte(`{"type":"result","sessionId":"s1","exitCode":0,"usage":{"premiumRequests":7.5}}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}
	if ev.Result == nil || ev.Result.PremiumRequests != 7.5 {
		t.Fatalf("PremiumRequests = %+v, want 7.5", ev.Result)
	}
}

func TestParseCopilotLine_ResultNonZeroExitIsError(t *testing.T) {
	line := []byte(`{"type":"result","sessionId":"s2","exitCode":1,"usage":{"premiumRequests":1}}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}
	if ev.Type != "result" || ev.Subtype != "error" {
		t.Fatalf("Type/Subtype = %q/%q, want result/error", ev.Type, ev.Subtype)
	}
	if ev.Result == nil || ev.Result.ErrorStatus != 1 {
		t.Errorf("ErrorStatus = %v, want 1", ev.Result)
	}
	// A non-zero-exit result must NOT count as a clean terminal result, so
	// reattach completion does not treat a crashed run as success.
	se := copilotEventToStreamEvent(ev)
	if se.Subtype != "error" {
		t.Errorf("StreamEvent.Subtype = %q, want error", se.Subtype)
	}
}

func TestParseCopilotLine_ToolResultArrayContent(t *testing.T) {
	// Defensive: a structured (array) content must not drop the whole event.
	line := []byte(`{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true,"result":{"content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}}}`)
	ev, err := ParseCopilotLine(line)
	if err != nil {
		t.Fatalf("ParseCopilotLine: %v", err)
	}
	if ev.Type != "tool_result" || len(ev.Message.ToolResults) != 1 {
		t.Fatalf("want tool_result with one result, got %+v", ev)
	}
	if ev.Message.ToolResults[0].Content != "line1\nline2" {
		t.Errorf("content = %q, want line1\\nline2", ev.Message.ToolResults[0].Content)
	}
}

func TestCopilotEventToStreamEvent(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantTyp string
		check   func(t *testing.T, ev StreamEvent)
	}{
		{
			name:    "assistant",
			line:    `{"type":"assistant.message","data":{"content":"hi","toolRequests":[],"outputTokens":7}}`,
			wantTyp: "assistant",
			check: func(t *testing.T, ev StreamEvent) {
				t.Helper()
				if ev.Content != "hi" {
					t.Errorf("Content = %q, want hi", ev.Content)
				}
				if ev.OutputTokens != 7 {
					t.Errorf("OutputTokens = %d, want 7", ev.OutputTokens)
				}
			},
		},
		{
			name:    "tool_use",
			line:    `{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash","arguments":{"command":"ls"}}}`,
			wantTyp: "tool_use",
			check: func(t *testing.T, ev StreamEvent) {
				t.Helper()
				if ev.Content != "ls" {
					t.Errorf("Content = %q, want ls", ev.Content)
				}
			},
		},
		{
			name:    "tool_result",
			line:    `{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true,"result":{"content":"out"}}}`,
			wantTyp: "tool_result",
			check: func(t *testing.T, ev StreamEvent) {
				t.Helper()
				if ev.Content != "out" {
					t.Errorf("Content = %q, want out", ev.Content)
				}
			},
		},
		{
			name:    "result",
			line:    `{"type":"result","sessionId":"s1","usage":{"premiumRequests":2}}`,
			wantTyp: "result",
			check: func(t *testing.T, ev StreamEvent) {
				t.Helper()
				if ev.SessionID != "s1" {
					t.Errorf("SessionID = %q, want s1", ev.SessionID)
				}
				if ev.PremiumRequests != 2 {
					t.Errorf("PremiumRequests = %v, want 2", ev.PremiumRequests)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseCopilotLine([]byte(tt.line))
			if err != nil {
				t.Fatalf("ParseCopilotLine: %v", err)
			}
			ev := copilotEventToStreamEvent(parsed)
			if ev.Type != tt.wantTyp {
				t.Fatalf("Type = %q, want %q", ev.Type, tt.wantTyp)
			}
			tt.check(t, ev)
		})
	}
}

func TestNormalizeProviderCopilot(t *testing.T) {
	for _, in := range []string{"copilot", "Copilot", " COPILOT "} {
		if got := normalizeProvider(in); got != "copilot" {
			t.Errorf("normalizeProvider(%q) = %q, want copilot", in, got)
		}
	}
}

func TestNormalizeModelCopilot(t *testing.T) {
	tests := map[string]string{
		"":                copilotDefaultModel,
		"sonnet":          copilotDefaultModel,
		"opus":            copilotDefaultModel,
		"haiku":           copilotDefaultModel,
		"gpt-5.3-codex":   "gpt-5.3-codex",
		"claude-opus-4.6": "claude-opus-4.6",
		"auto":            "auto",
	}
	for in, want := range tests {
		if got := normalizeModel("copilot", in); got != want {
			t.Errorf("normalizeModel(copilot, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLogFile_Copilot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "copilot.ndjson")
	lines := `{"type":"session.tools_updated","data":{"model":"claude-sonnet-4.6"},"ephemeral":true}` + "\n" +
		`{"type":"assistant.message","data":{"content":"working","outputTokens":12}}` + "\n" +
		`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash","arguments":{"command":"ls"}}}` + "\n" +
		`{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true,"result":{"content":"out"}}}` + "\n" +
		`{"type":"result","sessionId":"s9","usage":{"premiumRequests":3}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	events, err := ParseLogFile(path, 0, "copilot")
	if err != nil {
		t.Fatalf("ParseLogFile: %v", err)
	}
	// ephemeral session line dropped; assistant + tool_use + tool_result + result kept.
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Type != "assistant" || events[0].Content != "working" {
		t.Errorf("event[0] = %+v, want assistant/working", events[0])
	}
	if events[3].Type != "result" || events[3].PremiumRequests != 3 || events[3].SessionID != "s9" {
		t.Errorf("event[3] = %+v, want result with premiumRequests=3 sessionID=s9", events[3])
	}
}

func TestBuildHeadlessInvocation_Copilot(t *testing.T) {
	a := &Agent{Provider: "copilot", Model: "gpt-5.4"}
	name, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "do it"})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}
	if name != "copilot" {
		t.Fatalf("name = %q, want copilot", name)
	}
	want := map[string]bool{"--output-format": true, "--allow-all-tools": true, "--no-ask-user": true}
	for _, a := range args {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Errorf("missing required args: %v (got %v)", want, args)
	}

	// With a captured session id, resume via --session-id.
	a.SessionID = "sid-1"
	_, args, _, _, _ = buildHeadlessInvocation(a, RunConfig{Prompt: "again"})
	var sawSession bool
	for i, arg := range args {
		if arg == "--session-id" && i+1 < len(args) && args[i+1] == "sid-1" {
			sawSession = true
		}
	}
	if !sawSession {
		t.Errorf("expected --session-id sid-1 in %v", args)
	}
}
