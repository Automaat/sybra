package sybra

import (
	"testing"
	"time"

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

func testOrchestratorAgent(id string, state agent.State, sessionID string, startedAt time.Time) *agent.Agent {
	a := &agent.Agent{
		ID:        id,
		Name:      orchestratorAgentName,
		StartedAt: startedAt,
	}
	a.SetState(state)
	a.SetSessionID(sessionID)
	return a
}

func TestSelectOrchestratorSingleton_AdoptsNewestAndStopsDuplicates(t *testing.T) {
	now := time.Now()
	keep, stop := selectOrchestratorSingleton("", []*agent.Agent{
		testOrchestratorAgent("old", agent.StatePaused, "sess-old", now.Add(-time.Hour)),
		testOrchestratorAgent("new", agent.StatePaused, "sess-new", now),
		testOrchestratorAgent("wedged", agent.StatePaused, "", now.Add(time.Hour)),
		{ID: "task-agent", Name: "implementation", State: agent.StateRunning, StartedAt: now.Add(2 * time.Hour)},
	})

	if keep != "new" {
		t.Fatalf("keep = %q, want new", keep)
	}
	assertSameStrings(t, stop, []string{"old", "wedged"})
}

func TestSelectOrchestratorSingleton_PreservesCurrentHealthy(t *testing.T) {
	now := time.Now()
	keep, stop := selectOrchestratorSingleton("old", []*agent.Agent{
		testOrchestratorAgent("old", agent.StatePaused, "sess-old", now.Add(-time.Hour)),
		testOrchestratorAgent("new", agent.StatePaused, "sess-new", now),
	})

	if keep != "old" {
		t.Fatalf("keep = %q, want old", keep)
	}
	assertSameStrings(t, stop, []string{"new"})
}

func TestSelectOrchestratorSingleton_StopsReplaceableWhenNoHealthyCandidate(t *testing.T) {
	now := time.Now()
	keep, stop := selectOrchestratorSingleton("wedged", []*agent.Agent{
		testOrchestratorAgent("wedged", agent.StatePaused, "", now),
		testOrchestratorAgent("stopped", agent.StateStopped, "sess-stopped", now.Add(-time.Hour)),
	})

	if keep != "" {
		t.Fatalf("keep = %q, want empty", keep)
	}
	assertSameStrings(t, stop, []string{"wedged", "stopped"})
}

func TestSelectOrchestratorSingleton_IgnoresHealthyCurrentNonOrchestrator(t *testing.T) {
	now := time.Now()
	keep, stop := selectOrchestratorSingleton("task-agent", []*agent.Agent{
		{ID: "task-agent", Name: "implementation", State: agent.StateRunning, StartedAt: now},
	})

	if keep != "" {
		t.Fatalf("keep = %q, want empty", keep)
	}
	if len(stop) != 0 {
		t.Fatalf("stop = %v, want empty", stop)
	}
}

func assertSameStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	counts := make(map[string]int, len(got))
	for _, v := range got {
		counts[v]++
	}
	for _, v := range want {
		if counts[v] == 0 {
			t.Fatalf("got %v, want %v", got, want)
		}
		counts[v]--
	}
}
