package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestDispatchPlanningWorkflow_RecoversEscalatedMissingStep pins the recovery
// contract the engine's missing-step escalation promises in its status reason.
//
// Deleting a workflow step strands any task parked on it, so the engine flips
// those to human-required with ExecFailed and tells the operator to set the task
// back to planning. That instruction is only true if the planning dispatcher
// accepts a failed execution — it rejects non-terminal ones — so this asserts
// the operator's documented escape actually starts a fresh workflow rather than
// silently no-opping.
func TestDispatchPlanningWorkflow_RecoversEscalatedMissingStep(t *testing.T) {
	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine

	created, err := app.tasks.Create("escalated off a deleted step", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status": string(task.StatusPlanning),
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "address_critique",
			State:       workflow.ExecFailed,
		},
	}); err != nil {
		t.Fatal(err)
	}

	app.dispatchPlanningWorkflow(created.ID)

	got, err := app.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("workflow missing after dispatch")
	}
	if got.Workflow.CurrentStep == "address_critique" {
		t.Fatal("re-plan no-opped: the task is still parked on the deleted step, so the escalation's advice is a dead end")
	}
	if got.Workflow.State == workflow.ExecFailed {
		t.Errorf("State = %q, want a fresh execution", got.Workflow.State)
	}
}
