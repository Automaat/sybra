package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

func TestAppInstanceRoleGates(t *testing.T) {
	ptr := func(b bool) *bool { return &b }

	tests := []struct {
		name          string
		orch          config.OrchestratorConfig
		wantScheduler bool
		wantBrain     bool
	}{
		{
			name:          "default full runs both",
			orch:          config.DefaultConfig().Orchestrator,
			wantScheduler: true,
			wantBrain:     true,
		},
		{
			name:          "agent-only runs neither",
			orch:          config.OrchestratorConfig{Role: config.InstanceRoleAgentOnly},
			wantScheduler: false,
			wantBrain:     false,
		},
		{
			name:          "agent-only with explicit scheduler",
			orch:          config.OrchestratorConfig{Role: config.InstanceRoleAgentOnly, SchedulerEnabled: ptr(true)},
			wantScheduler: true,
			wantBrain:     false,
		},
		{
			name:          "invalid role falls back to full",
			orch:          config.OrchestratorConfig{Role: "bogus"},
			wantScheduler: true,
			wantBrain:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &App{cfg: &config.Config{Orchestrator: tt.orch}}
			if got := a.runsScheduler(); got != tt.wantScheduler {
				t.Errorf("runsScheduler() = %v, want %v", got, tt.wantScheduler)
			}
			if got := a.runsOrchestratorBrain(); got != tt.wantBrain {
				t.Errorf("runsOrchestratorBrain() = %v, want %v", got, tt.wantBrain)
			}
		})
	}
}

func TestAppInstanceRoleGatesNilConfig(t *testing.T) {
	a := &App{}
	if !a.runsScheduler() {
		t.Error("runsScheduler() = false with nil cfg, want true")
	}
	if !a.runsOrchestratorBrain() {
		t.Error("runsOrchestratorBrain() = false with nil cfg, want true")
	}
}

func TestAgentOnlyQueueDrainPassIsNoop(t *testing.T) {
	a := setupManualQueueApp(t, "", "", 1)
	a.cfg.Orchestrator.Role = config.InstanceRoleAgentOnly

	blocker := createResearchTaskWithPriority(t, a.tasks, "blocker", task.PriorityMedium)
	if _, err := a.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false); err != nil {
		t.Fatalf("StartAgent(blocker): %v", err)
	}
	queued := createResearchTaskWithPriority(t, a.tasks, "queued", task.PriorityMedium)
	if _, err := a.StartAgent(queued.ID, "headless", "queued", false); err != nil {
		t.Fatalf("StartAgent(queued): %v", err)
	}
	if got := len(a.agentQueue.Snapshot()); got != 1 {
		t.Fatalf("queue depth before drain = %d, want 1", got)
	}

	a.queueDrainPass(t.Context())

	if got := len(a.agentQueue.Snapshot()); got != 1 {
		t.Fatalf("queue depth after agent-only drain = %d, want 1 (drain must not run)", got)
	}
}
