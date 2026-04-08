package events

import "testing"

func TestAgentEventFunctions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func(string) string
		want string
	}{
		{"AgentState", AgentState, AgentStatePrefix + "abc123"},
		{"AgentOutput", AgentOutput, AgentOutputPrefix + "abc123"},
		{"AgentError", AgentError, AgentErrorPrefix + "abc123"},
		{"AgentStuck", AgentStuck, AgentStuckPrefix + "abc123"},
		{"AgentConvo", AgentConvo, AgentConvoPrefix + "abc123"},
		{"AgentApproval", AgentApproval, AgentApprovalPrefix + "abc123"},
		{"AgentEscalation", AgentEscalation, AgentEscalationPrefix + "abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.fn("abc123")
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
