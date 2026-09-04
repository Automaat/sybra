package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
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
		Status:          task.Ptr(task.StatusHumanRequired),
		Escalation:      task.OperatorDecisionEvidence("test.fixture_human_required", "test fixture"),
		AutonomyOutcome: task.HumanRequiredOutcome(),
		StatusReason:    task.Ptr("existing reason"),
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

func TestTaskAdapterUpdateTaskStatusPreservesSameStatusReasonWhenReasonEmpty(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("preserve same-status reason", "", "headless", task.Update{
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr("watchdog bounded retry armed"),
		Blocker: task.Ptr(blocker.State{
			Kind:  blocker.KindWatchdogRateLimitExhausted,
			Actor: blocker.ActorWorkflow,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	if err := ta.UpdateTaskStatus(created.ID, task.StatusBlocked, ""); err != nil {
		t.Fatal(err)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.StatusReason != "watchdog bounded retry armed" {
		t.Fatalf("StatusReason = %q, want preserved watchdog marker", updated.StatusReason)
	}
}

func TestTaskAdapterUpdateTaskStatusClearsReasonOnStatusChangeWhenReasonEmpty(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("clear transitioned reason", "", "headless", task.Update{
		Status:          task.Ptr(task.StatusHumanRequired),
		Escalation:      task.OperatorDecisionEvidence("test.fixture_human_required", "test fixture"),
		AutonomyOutcome: task.HumanRequiredOutcome(),
		StatusReason:    task.Ptr("manual verification blocker: waiting for linked PR"),
		PRNumber:        task.Ptr(42),
	})
	if err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	if err := ta.UpdateTaskStatus(created.ID, task.StatusInReview, ""); err != nil {
		t.Fatal(err)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInReview {
		t.Fatalf("Status = %q, want %q", updated.Status, task.StatusInReview)
	}
	if updated.StatusReason != "" {
		t.Fatalf("StatusReason = %q, want empty after status transition", updated.StatusReason)
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

// TestTaskAdapterSetBlockerAndWorkflowPersistsAllFieldsAtomically guards
// #2749's blocker-path counterpart to SetStatusAndWorkflow: a caller
// escalating to "blocked" with a workflow-owned blocker.State alongside a
// workflow mutation must land Status, Blocker, and Workflow in one store
// write, not a status+blocker write followed by a separate SetWorkflow call.
func TestTaskAdapterSetBlockerAndWorkflowPersistsAllFieldsAtomically(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.Create("blocker+workflow", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wf := &workflow.Execution{WorkflowID: "wf-1", State: workflow.ExecRunning, CurrentStep: "run_test"}
	if _, err := tasks.Update(created.ID, task.Update{Workflow: &wf}); err != nil {
		t.Fatal(err)
	}
	before, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: tasks}
	terminal := &workflow.Execution{WorkflowID: "wf-1", State: workflow.ExecFailed, CurrentStep: ""}
	state := blocker.State{Kind: blocker.KindWatchdogRateLimitExhausted, Actor: blocker.ActorWorkflow, Exhausted: true}
	if err := ta.SetBlockerAndWorkflow(created.ID, "blocked", "retry budget exhausted", state, terminal); err != nil {
		t.Fatal(err)
	}

	after, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != task.StatusBlocked {
		t.Fatalf("Status = %q, want %q", after.Status, task.StatusBlocked)
	}
	if after.StatusReason != "retry budget exhausted" {
		t.Fatalf("StatusReason = %q, want %q", after.StatusReason, "retry budget exhausted")
	}
	if after.Blocker != state {
		t.Fatalf("Blocker = %+v, want %+v", after.Blocker, state)
	}
	if after.Workflow == nil || after.Workflow.State != workflow.ExecFailed {
		t.Fatalf("Workflow.State = %v, want %v", after.Workflow, workflow.ExecFailed)
	}
	// Exactly one generation bump for the whole call — proof it was a single
	// store write, not a status+blocker write followed by a second one.
	if after.Generation != before.Generation+1 {
		t.Fatalf("Generation = %d, want %d (single atomic write)", after.Generation, before.Generation+1)
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
