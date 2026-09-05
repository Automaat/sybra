package task

import (
	"testing"

	"github.com/Automaat/sybra/internal/workflow"
)

func TestApplyUpdate_ClearWorkflowRemovesTheExecution(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	created, err := store.Create("wedged", "", AgentModeHeadless)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Update(created.ID, Update{Workflow: Ptr(&workflow.Execution{WorkflowID: "simple-task-implement", State: workflow.ExecRunning})}); err != nil {
		t.Fatalf("attach workflow: %v", err)
	}
	cleared, err := store.Update(created.ID, Update{ClearWorkflow: Ptr(true)})
	if err != nil {
		t.Fatalf("clear workflow: %v", err)
	}
	if cleared.Workflow != nil {
		t.Errorf("Workflow = %+v, want nil", cleared.Workflow)
	}
}

// TestApplyUpdate_ClearWorkflowWinsOverAnAttach pins the precedence the CLI
// relies on: reopen sends a clear alongside the other field resets, and an
// Update carrying both must not resurrect the execution it is discarding.
func TestApplyUpdate_ClearWorkflowWinsOverAnAttach(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	created, err := store.Create("both set", "", AgentModeHeadless)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cleared, err := store.Update(created.ID, Update{
		ClearWorkflow: Ptr(true),
		Workflow:      Ptr(&workflow.Execution{WorkflowID: "simple-task-plan", State: workflow.ExecRunning}),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if cleared.Workflow != nil {
		t.Errorf("Workflow = %+v, want the clear to win", cleared.Workflow)
	}
}
