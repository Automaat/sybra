package sybra

import (
	"testing"

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
