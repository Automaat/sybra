package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
)

func TestOrchestratorReplaceable(t *testing.T) {
	tests := []struct {
		name      string
		state     agent.State
		sessionID string
		want      bool
	}{
		{"stopped is replaceable", agent.StateStopped, "", true},
		{"stopped with session is replaceable", agent.StateStopped, "sess-1", true},
		{"paused with no session is the wedged brain", agent.StatePaused, "", true},
		{"paused with session is healthy between turns", agent.StatePaused, "sess-1", false},
		{"running is healthy", agent.StateRunning, "", false},
		{"idle is healthy", agent.StateIdle, "sess-1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &agent.Agent{}
			a.SetState(tt.state)
			a.SetSessionID(tt.sessionID)
			if got := orchestratorReplaceable(a); got != tt.want {
				t.Errorf("orchestratorReplaceable(state=%s, session=%q) = %v, want %v",
					tt.state, tt.sessionID, got, tt.want)
			}
		})
	}
}
