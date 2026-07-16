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

func TestParseOpenCodeRunTextAndStepFinish(t *testing.T) {
	text, err := ParseOpenCodeLine([]byte(`{"type":"text","sessionID":"sess-4","part":{"type":"text","text":"k8s-opencode-ok"}}`))
	if err != nil {
		t.Fatalf("text parse: %v", err)
	}
	se := opencodeEventToStreamEvent(text)
	if se.Type != "assistant" || se.SessionID != "sess-4" || se.Content != "k8s-opencode-ok" {
		t.Fatalf("text stream event = %+v", se)
	}

	done, err := ParseOpenCodeLine([]byte(`{"type":"step_finish","sessionID":"sess-4","part":{"type":"step-finish","reason":"stop","tokens":{"total":7065,"input":1662,"output":8,"reasoning":15,"cache":{"write":0,"read":5380}},"cost":0.00258306}}`))
	if err != nil {
		t.Fatalf("step_finish parse: %v", err)
	}
	se = opencodeEventToStreamEvent(done)
	if se.Type != "result" || se.SessionID != "sess-4" || se.InputTokens != 1662 || se.OutputTokens != 8 || se.CacheReadInputTokens != 5380 || se.ReasoningTokens != 15 || se.CostUSD != 0.00258306 {
		t.Fatalf("step_finish stream event = %+v", se)
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

	failed, err = ParseOpenCodeLine([]byte(`{"type":"error","sessionID":"sess-3","error":{"name":"APIError","data":{"message":"User not found.","statusCode":401}}}`))
	if err != nil {
		t.Fatalf("object error parse: %v", err)
	}
	se = opencodeEventToStreamEvent(failed)
	if se.Type != "result" || se.Subtype != "error" || se.SessionID != "sess-3" || se.Content != "User not found." || se.ErrorType != "APIError" || se.ErrorStatus != 401 {
		t.Fatalf("object error stream event = %+v", se)
	}
}
