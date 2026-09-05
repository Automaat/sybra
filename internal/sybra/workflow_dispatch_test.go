package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

func TestHasApprovedPlanContractEmpty(t *testing.T) {
	tsk := task.Task{ID: "t1", Status: task.StatusTodo}
	if hasApprovedPlanContract(tsk) {
		t.Error("expected a brand-new todo task with no plan contract to report false")
	}
}

func TestHasApprovedPlanContractValid(t *testing.T) {
	tsk := task.Task{ID: "t1", Status: task.StatusTodo, PlanContract: validTestPlanContract("t1")}
	if !hasApprovedPlanContract(tsk) {
		t.Error("expected a todo task with a structurally valid plan contract to report true")
	}
}

func TestHasApprovedPlanContractInvalid(t *testing.T) {
	tsk := task.Task{ID: "t1", Status: task.StatusTodo, PlanContract: "not json"}
	if hasApprovedPlanContract(tsk) {
		t.Error("expected a todo task with a malformed plan contract to report false")
	}
}

func validTestPlanContract(taskID string) string {
	return `{
  "task_id": "` + taskID + `",
  "branch": "feat/example-` + taskID + `",
  "worktree": "/home/sybra/.sybra/worktrees/example-` + taskID + `",
  "files": [{"path": "internal/workflow/engine.go", "purpose": "edit"}],
  "steps": ["wire the contract through the workflow"],
  "verification": [{"command": "go test ./internal/workflow", "expected": "tests pass"}],
  "acceptance_criteria": ["implementation prompt includes the contract"],
  "risk_tier": "medium",
  "permission_tier": "repo-write",
  "rollback": "revert the workflow and sidecar changes"
}`
}

func TestSkipTaskCreatedWorkflowUmbrellaGated(t *testing.T) {
	tsk := task.Task{Tags: []string{umbrella.GatedTag}}
	if !skipTaskCreatedWorkflow(tsk) {
		t.Errorf("expected umbrella-gated task to skip task:created dispatch")
	}
}

func TestSkipTaskCreatedWorkflowUngated(t *testing.T) {
	tsk := task.Task{Tags: []string{"backend", "medium"}}
	if skipTaskCreatedWorkflow(tsk) {
		t.Errorf("expected ungated task not to skip task:created dispatch")
	}
}

func TestSkipTaskCreatedWorkflowUmbrellaTaskType(t *testing.T) {
	tsk := task.Task{TaskType: task.TaskTypeUmbrella, Tags: []string{"backend"}}
	if !skipTaskCreatedWorkflow(tsk) {
		t.Error("expected a TaskType=umbrella task to skip task:created dispatch, even without a marker tag")
	}
}

func TestSkipTaskCreatedWorkflowPromptLabProposal(t *testing.T) {
	tsk := task.Task{Status: task.StatusTodo, Tags: []string{promptlab.ProposalTag, "role:review"}}
	if !skipTaskCreatedWorkflow(tsk) {
		t.Error("expected prompt-lab proposal task to skip task:created dispatch")
	}
}

func TestSkipTaskCreatedWorkflowInboundReviewTodo(t *testing.T) {
	tsk := task.Task{
		Status:   task.StatusTodo,
		PRNumber: 42,
		Tags:     []string{"review"},
	}
	if skipTaskCreatedWorkflow(tsk) {
		t.Error("expected a newly created inbound review task to enter task:created dispatch")
	}
}

func TestSkipTaskCreatedWorkflowPlanningReviewWithPR(t *testing.T) {
	tsk := task.Task{
		Status:   task.StatusPlanning,
		PRNumber: 42,
		Tags:     []string{"backend", "review"},
	}
	if !skipTaskCreatedWorkflow(tsk) {
		t.Error("expected a planning task with a linked PR to stay out of task:created dispatch")
	}
}

func TestTaskToInfo_PreservesRunRole(t *testing.T) {
	info := taskToInfo(task.Task{ID: "t1", RunRole: "review"})
	if info.Role != "review" {
		t.Fatalf("Role = %q, want review", info.Role)
	}
}
