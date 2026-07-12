package workflow

import "testing"

func stubCrossAvailability(t *testing.T) {
	t.Helper()
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })
}

func TestResolveProviderCrossUsesCurrentWorkflowHistory(t *testing.T) {
	stubCrossAvailability(t)
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
	stubCrossAvailability(t)
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
	stubCrossAvailability(t)
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

func TestResolveProviderCrossRotatesCodexToCopilot(t *testing.T) {
	stubCrossAvailability(t)
	tk := TaskInfo{HandoffSourceProvider: "codex"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "copilot" {
		t.Fatalf("provider = %q, want copilot (codex rotates to copilot)", got)
	}
}

func TestResolveProviderCrossRotatesCopilotToClaude(t *testing.T) {
	stubCrossAvailability(t)
	tk := TaskInfo{HandoffSourceProvider: "copilot"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "claude" {
		t.Fatalf("provider = %q, want claude", got)
	}
}

func TestResolveProviderCrossSkipsUnavailableRotationTarget(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(p string) bool { return p != "copilot" }
	t.Cleanup(func() { providerAvailable = prev })
	tk := TaskInfo{HandoffSourceProvider: "codex"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "claude" {
		t.Fatalf("provider = %q, want claude (copilot unavailable, rotate past it)", got)
	}
}

func TestResolveProviderCrossAllUnavailableReturnsFirstDifferent(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return false }
	t.Cleanup(func() { providerAvailable = prev })
	tk := TaskInfo{HandoffSourceProvider: "codex"}

	got := resolveProvider("cross", &Execution{}, "codex", tk)
	if got != "copilot" {
		t.Fatalf("provider = %q, want copilot (first different in rotation; resolveAgentVariant strips it to default)", got)
	}
}

func TestResolveProviderCrossWithoutProvenanceDefersToDefaultProvider(t *testing.T) {
	stubCrossAvailability(t)
	got := resolveProvider("cross", &Execution{}, "claude", TaskInfo{})
	if got != "" {
		t.Fatalf("provider = %q, want empty default-provider sentinel", got)
	}
}
