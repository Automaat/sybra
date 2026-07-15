package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

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
