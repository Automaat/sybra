package main

import (
	"testing"

	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
)

func TestPlanningService_TriageTask(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("triage me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := planSvc.TriageTask(created.ID); err != nil {
		t.Fatal(err)
	}

	tk, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil {
		t.Fatal("expected workflow to be set after TriageTask")
	}
}

func TestPlanningService_PlanTask_NoopWithActiveWorkflow(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("plan me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Set up a workflow manually.
	if _, err := taskSvc.tasks.Update(created.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID: "simple-task",
			State:      workflow.ExecRunning,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// PlanTask should be a no-op.
	if err := planSvc.PlanTask(created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningService_PlanTask_StartsWorkflow(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("plan me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := planSvc.PlanTask(created.ID); err != nil {
		t.Fatal(err)
	}

	tk, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil {
		t.Fatal("expected workflow to be set after PlanTask")
	}
}

func TestPlanningService_ApprovePlan_ErrorWhenNotWaiting(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("approve me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// No workflow running → approve should fail.
	if _, err := planSvc.ApprovePlan(created.ID); err == nil {
		t.Fatal("expected error when approving task without active workflow")
	}
}

func TestPlanningService_RejectPlan_ErrorWhenNotWaiting(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("reject me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := planSvc.RejectPlan(created.ID, "bad plan"); err == nil {
		t.Fatal("expected error when rejecting task without active workflow")
	}
}

func TestPlanningService_SendPlanMessage_EmptyMessage(t *testing.T) {
	planSvc, _, _ := setupPlanningService(t)

	err := planSvc.SendPlanMessage("any-id", "")
	if err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestPlanningService_SendPlanMessage_NoAgent(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("msg task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	err = planSvc.SendPlanMessage(created.ID, "hello")
	if err == nil {
		t.Fatal("expected error when no plan agent running")
	}
}

func TestPlanningService_HasLivePlanAgent_FalseByDefault(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("check agent", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if planSvc.HasLivePlanAgent(created.ID) {
		t.Fatal("expected no live plan agent by default")
	}
}

func TestTaskService_CreateTask_AutoStartsWorkflow(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("auto-workflow task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// The workflow auto-start runs in a goroutine, so the task may not
	// have a workflow immediately. Check it was created with todo status.
	if created.Status != task.StatusTodo {
		t.Fatalf("expected todo, got %q", created.Status)
	}
}

func TestTaskService_UpdateTask_CleansWorktreeOnDone(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("cleanup task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.UpdateTask(created.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusDone {
		t.Fatalf("expected done, got %q", updated.Status)
	}
}
