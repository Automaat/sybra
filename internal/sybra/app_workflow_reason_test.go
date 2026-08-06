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
	if err := ta.UpdateTaskStatus(created.ID, task.StatusHumanRequired, ""); err != nil {
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
	if err := ta.UpdateTaskStatus(created.ID, task.StatusHumanRequired, ""); err != nil {
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

func TestTaskAdapterClearTaskStatusReasonIfIsAtomicGuard(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("guard reason", "", "headless", task.Update{
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr("watchdog: rate limit: quota exhausted"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	cleared, err := ta.ClearTaskStatusReasonIf(created.ID, task.StatusInProgress, "stale marker")
	if err != nil || cleared {
		t.Fatalf("mismatched clear = (%v, %v), want (false, nil)", cleared, err)
	}
	current, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.StatusReason != "watchdog: rate limit: quota exhausted" {
		t.Fatalf("mismatched clear overwrote reason %q", current.StatusReason)
	}

	cleared, err = ta.ClearTaskStatusReasonIf(created.ID, task.StatusInProgress, current.StatusReason)
	if err != nil || !cleared {
		t.Fatalf("matched clear = (%v, %v), want (true, nil)", cleared, err)
	}
	current, err = tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != task.StatusInProgress || current.StatusReason != "" {
		t.Fatalf("matched clear left status=%q reason=%q, want in-progress with empty reason", current.Status, current.StatusReason)
	}
}
