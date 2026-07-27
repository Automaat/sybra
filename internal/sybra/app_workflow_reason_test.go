package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestTaskAdapterUpdateTaskStatusPreservesExistingHumanRequiredReason(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("preserve reason", "", "headless", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("existing reason"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	if err := ta.UpdateTaskStatus(created.ID, string(task.StatusHumanRequired), ""); err != nil {
		t.Fatal(err)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.StatusReason != "existing reason" {
		t.Fatalf("StatusReason = %q, want %q", updated.StatusReason, "existing reason")
	}
}

func TestTaskAdapterUpdateTaskStatusSynthesizesHumanRequiredReasonFromLatestRun(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.Create("synthesize reason", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusPlanning)}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-plan",
		Role:      "plan",
		Mode:      "headless",
		State:     "stopped",
		StartedAt: time.Now().Add(-2 * time.Minute).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "plan",
		State:       workflow.ExecWaiting,
	}
	if _, err := tasks.Update(created.ID, task.Update{Workflow: &wf}); err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	if err := ta.UpdateTaskStatus(created.ID, string(task.StatusHumanRequired), ""); err != nil {
		t.Fatal(err)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const want = "plan agent run ag-plan at workflow step plan stopped without a recorded outcome or result"
	if updated.StatusReason != want {
		t.Fatalf("StatusReason = %q, want %q", updated.StatusReason, want)
	}
}
