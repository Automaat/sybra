package workflow

import "testing"

func TestAdversarialFixSuggestionRejected(t *testing.T) {
	t.Parallel()
	output := "## Test Failures\n\nCommand run: go test ./internal/workflow\nActual output: FAIL\nExpected: PASS\nCode evidence: engine_steps_testroute.go: change AgentRun.Verdict to VerdictRendered\nTEST_VERDICT: FAIL\n"
	if got := containsFixSuggestions(output); !got {
		t.Errorf("containsFixSuggestions(%q) = false, want true", output)
	}
}
