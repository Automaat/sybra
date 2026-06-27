package workflow

import "testing"

func TestResolveProviderCrossUsesCurrentWorkflowHistory(t *testing.T) {
	wf := &Execution{StepHistory: []StepRecord{
		{StepID: "implement", Provider: "claude"},
		{StepID: "set_ready_review"},
	}}
	tk := TaskInfo{
		HandoffSourceProvider: "codex",
		AgentRuns: []AgentRunInfo{
			{Role: "implementation", Provider: "codex"},
		},
	}

	got := resolveProvider("cross", wf, "claude", tk)
	if got != "codex" {
		t.Fatalf("provider = %q, want codex", got)
	}
}

func TestResolveProviderCrossUsesCodeAuthorRunBeforeHandoffSource(t *testing.T) {
	tk := TaskInfo{
		HandoffSourceProvider: "codex",
		AgentRuns: []AgentRunInfo{
			{Role: "implementation", Provider: "claude"},
		},
	}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "codex" {
		t.Fatalf("provider = %q, want codex", got)
	}
}

func TestResolveProviderCrossIgnoresReviewRunsForTaskProvenance(t *testing.T) {
	tk := TaskInfo{
		AgentRuns: []AgentRunInfo{
			{Role: "implementation", Provider: "claude"},
			{Role: "review", Provider: "codex"},
		},
	}

	got := resolveProvider("cross", &Execution{}, "claude", tk)
	if got != "codex" {
		t.Fatalf("provider = %q, want codex", got)
	}
}

func TestResolveProviderCrossUsesHandoffSourceProvider(t *testing.T) {
	tk := TaskInfo{HandoffSourceProvider: "codex"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "claude" {
		t.Fatalf("provider = %q, want claude", got)
	}
}

func TestResolveProviderCrossCopilotFallsBackToClaude(t *testing.T) {
	tk := TaskInfo{HandoffSourceProvider: "copilot"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "claude" {
		t.Fatalf("provider = %q, want claude", got)
	}
}

func TestResolveProviderCrossWithoutProvenanceDefersToDefaultProvider(t *testing.T) {
	got := resolveProvider("cross", &Execution{}, "claude", TaskInfo{})
	if got != "" {
		t.Fatalf("provider = %q, want empty default-provider sentinel", got)
	}
}
