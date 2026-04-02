package agent

import (
	"context"
	"testing"

	"github.com/Automaat/synapse/internal/tmux"
)

func newTestManager(t *testing.T) (mgr *Manager, events *[]string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	emitted := &[]string{}
	emit := func(event string, _ any) {
		*emitted = append(*emitted, event)
	}

	m := NewManager(ctx, tmux.NewManager(), emit)
	return m, emitted
}

func TestNewManager(t *testing.T) {
	m, _ := newTestManager(t)
	if m == nil {
		t.Fatal("manager is nil")
	}
	if len(m.ListAgents()) != 0 {
		t.Error("expected empty agent list")
	}
}

func TestStartAgentUnknownMode(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.StartAgent("task-1", "invalid", "prompt", nil)
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestStartAgentHeadless(t *testing.T) {
	m, emitted := newTestManager(t)

	// Start headless agent — will fail to run claude but agent entry is created
	a, err := m.StartAgent("task-1", "headless", "test prompt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if a.ID == "" {
		t.Error("agent ID is empty")
	}
	if a.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want %q", a.TaskID, "task-1")
	}
	if a.Mode != "headless" {
		t.Errorf("Mode = %q, want %q", a.Mode, "headless")
	}
	if a.State != StateRunning {
		t.Errorf("State = %q, want %q", a.State, StateRunning)
	}

	agents := m.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}

	if len(*emitted) == 0 {
		t.Error("expected at least one emitted event")
	}
}

func TestGetAgent(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.GetAgent(a.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.GetAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestStopAgent(t *testing.T) {
	m, emitted := newTestManager(t)

	a, err := m.StartAgent("task-1", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	got, err := m.GetAgent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateStopped {
		t.Errorf("State = %q, want %q", got.State, StateStopped)
	}

	// Should have emitted state events for start and stop
	hasStop := false
	for _, e := range *emitted {
		if e == "agent:state:"+a.ID {
			hasStop = true
		}
	}
	if !hasStop {
		t.Error("expected agent:state event")
	}
}

func TestStopAgentNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	err := m.StopAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestListAgentsMultiple(t *testing.T) {
	m, _ := newTestManager(t)

	for i := range 3 {
		_, err := m.StartAgent("task-"+string(rune('1'+i)), "headless", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	agents := m.ListAgents()
	if len(agents) != 3 {
		t.Errorf("got %d agents, want 3", len(agents))
	}
}

func TestAgentOutput(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty
	if len(a.Output()) != 0 {
		t.Error("expected empty output buffer")
	}
}

func TestShutdown(t *testing.T) {
	m, _ := newTestManager(t)

	for range 3 {
		_, err := m.StartAgent("task-1", "headless", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	m.Shutdown()

	// All agents should still be in the map (shutdown doesn't remove them)
	if len(m.ListAgents()) != 3 {
		t.Errorf("got %d agents, want 3", len(m.ListAgents()))
	}
}
