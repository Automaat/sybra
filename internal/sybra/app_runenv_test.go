package sybra

import (
	"context"
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/task"
)

func TestAgentRunEnvironmentFailedReadsExitAndStreamDiagnostics(t *testing.T) {
	tests := []struct {
		name  string
		agent func() *agent.Agent
		want  bool
	}{
		{name: "clean", agent: func() *agent.Agent { return &agent.Agent{} }, want: false},
		{name: "exit error", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.SetExitErr(errors.New("fatal: read-only file system"))
			return ag
		}, want: true},
		{name: "terminal content", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "commit failed: gpg failed to sign the data"})
			return ag
		}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentRunEnvironmentFailed(tt.agent()); got != tt.want {
				t.Fatalf("agentRunEnvironmentFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuarantineRunEnvironmentParksMachineOwnedTask(t *testing.T) {
	a := setupApp(t)
	created, err := a.tasks.Create("quarantine me", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	a.quarantineRunEnvironment(context.Background(), runenv.CertificationError{
		TaskID: created.ID, Action: "implementation.dispatch", Scope: "task", Code: "checkout_unhealthy", Capability: autonomy.CapabilityCheckoutHealth,
	})
	got, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusBlocked || got.AutonomyOutcome != autonomy.OutcomeQuarantined {
		t.Fatalf("quarantined task status/outcome = %q/%q", got.Status, got.AutonomyOutcome)
	}
}
