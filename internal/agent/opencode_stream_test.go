package agent

import "testing"

func TestOpenCodeProviderNormalizeModel(t *testing.T) {
	tests := map[string]string{
		"":       opencodeDefaultModel,
		"sonnet": opencodeDefaultModel,
		"opus":   "openrouter/z-ai/glm-5.2",
		"haiku":  "openrouter/qwen/qwen3-32b",
	}
	for in, want := range tests {
		if got := normalizeModel("opencode", in); got != want {
			t.Errorf("normalizeModel(opencode, %q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeModel("opencode", "openrouter/qwen/qwen3-coder"); got != "openrouter/qwen/qwen3-coder" {
		t.Errorf("explicit model changed: %q", got)
	}
}

func TestOpenCodeBuildHeadlessInvocation(t *testing.T) {
	a := &Agent{Provider: "opencode", Model: "openrouter/z-ai/glm-5.2", ReasoningEffort: "high", sessionCWD: "/tmp/wt"}
	inv, err := providerByName("opencode").BuildHeadlessInvocation(a, RunConfig{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.name != "opencode" {
		t.Fatalf("name = %q, want opencode", inv.name)
	}
	want := []string{"run", "--format", "json", "--auto", "--model", "openrouter/z-ai/glm-5.2", "--variant", "high", "--dir", "/tmp/wt", "do it"}
	if len(inv.args) != len(want) {
		t.Fatalf("args = %#v, want %#v", inv.args, want)
	}
	for i := range want {
		if inv.args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; all=%#v", i, inv.args[i], want[i], inv.args)
		}
	}
}

func TestParseOpenCodeLineAssistantAndTool(t *testing.T) {
	assistant, err := ParseOpenCodeLine([]byte(`{"type":"assistant.message","sessionId":"sess-1","data":{"content":"hi","outputTokens":3,"toolRequests":[{"toolCallId":"call-1","name":"bash","arguments":{"command":"go test ./..."}}]}}`))
	if err != nil {
		t.Fatalf("assistant parse: %v", err)
	}
	se := opencodeEventToStreamEvent(assistant)
	if se.Type != "assistant" || se.Content != "hi" || se.SessionID != "sess-1" || se.OutputTokens != 3 || se.ToolCalls != 1 {
		t.Fatalf("assistant stream event = %+v", se)
	}

	toolResult, err := ParseOpenCodeLine([]byte(`{"type":"tool.execution_complete","data":{"toolCallId":"call-1","success":false,"result":"failed"}}`))
	if err != nil {
		t.Fatalf("tool parse: %v", err)
	}
	ce := opencodeEventToConvoEvent(toolResult)
	if ce.Type != "user" || len(ce.ToolResults) != 1 || ce.ToolResults[0].ToolUseID != "call-1" || !ce.ToolResults[0].IsError {
		t.Fatalf("tool convo event = %+v", ce)
	}
}

func TestParseOpenCodeLineResultUsageAndError(t *testing.T) {
	result, err := ParseOpenCodeLine([]byte(`{"type":"result","sessionId":"sess-2","message":"done","usage":{"inputTokens":10,"outputTokens":4,"cacheReadInputTokens":6,"reasoningTokens":2,"costUSD":0.01}}`))
	if err != nil {
		t.Fatalf("result parse: %v", err)
	}
	se := opencodeEventToStreamEvent(result)
	if se.Type != "result" || se.SessionID != "sess-2" || se.Content != "done" || se.InputTokens != 10 || se.OutputTokens != 4 || se.CacheReadInputTokens != 6 || se.ReasoningTokens != 2 || se.CostUSD != 0.01 {
		t.Fatalf("result stream event = %+v", se)
	}

	failed, err := ParseOpenCodeLine([]byte(`{"type":"error","errorType":"rate_limit","code":429,"message":"too many requests"}`))
	if err != nil {
		t.Fatalf("error parse: %v", err)
	}
	se = opencodeEventToStreamEvent(failed)
	if se.Type != "result" || se.Subtype != "error" || se.ErrorType != "rate_limit" || se.ErrorStatus != 429 {
		t.Fatalf("error stream event = %+v", se)
	}
}
