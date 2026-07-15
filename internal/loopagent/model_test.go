package loopagent

import "testing"

func TestAgentName(t *testing.T) {
	la := LoopAgent{Name: "self-monitor"}
	if got := la.AgentName(); got != "loop:self-monitor" {
		t.Fatalf("AgentName() = %q, want loop:self-monitor", got)
	}
}

func TestValidateEmptyProviderAccepted(t *testing.T) {
	la := LoopAgent{Name: "x", Prompt: "/p", IntervalSec: 60, Provider: ""}
	if err := la.Validate(); err != nil {
		t.Fatalf("empty provider should be accepted: %v", err)
	}
}
