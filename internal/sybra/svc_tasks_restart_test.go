package sybra

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestUpdateTask_InProgressDispatchesByTrigger verifies that moving a task
// to in-progress with a terminal workflow does NOT verbatim-restart the saved
// workflow_id (the bug for tasks created on the pre-split monolithic
// `simple-task`, which would re-run triage and flip status back to planning).
// Instead the engine must dispatch via task.status_changed so trigger
// conditions select the correct workflow — for in-progress this is
// simple-task-implement.
func TestUpdateTask_InProgressDispatchesByTrigger(t *testing.T) {
	svc, a := setupTaskService(t)

	// Create the task via the manager so CreateTask's auto-start workflow
	// goroutine doesn't race with our setup.
	tk, err := a.tasks.Create("retry me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Attach a fake completed workflow with the OLD pre-split ID. The
	// previous restart code would replay this verbatim; the fix dispatches
	// via task.status_changed and matches the post-split implement workflow.
	staleWf := &workflow.Execution{
		WorkflowID:  "simple-task",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
	}
	if _, err := a.tasks.Update(tk.ID, task.Update{Workflow: &staleWf}); err != nil {
		t.Fatal(err)
	}
	// Plan must be present so simple-task-implement has something to feed
	// the implement agent (matches the post-planning state of fa6919fc).
	if _, err := a.tasks.Update(tk.ID, task.Update{
		Plan:            task.Ptr("# Plan\n\nDo the thing.\n"),
		Status:          task.Ptr(task.StatusHumanRequired),
		Escalation:      task.OperatorDecisionEvidence("test.fixture_human_required", "test fixture"),
		AutonomyOutcome: task.HumanRequiredOutcome(),
	}); err != nil {
		t.Fatal(err)
	}

	// Move to in-progress. The restart goroutine fires DispatchEvent with
	// task.status_changed. Agent start may fail in the test environment
	// (no real claude binary), but the workflow_id is persisted on the
	// task BEFORE agent start, so the assertion below is meaningful.
	if _, err := svc.UpdateTask(tk.ID, map[string]any{"status": "in-progress"}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("workflow not set after restart")
	}
	if got.Workflow.WorkflowID == "simple-task" {
		t.Errorf("workflow restarted as stale ID %q — restart should dispatch via trigger conditions, not replay the saved id",
			got.Workflow.WorkflowID)
	}
	if got.Workflow.WorkflowID != "simple-task-implement" {
		t.Errorf("workflow.WorkflowID = %q, want simple-task-implement", got.Workflow.WorkflowID)
	}
}

// TestUpdateTask_InProgressNoWorkflowNoOp verifies the restart logic is
// guarded by `cur.Workflow != nil`: moving a brand-new task to in-progress
// (no prior workflow attached) must not blow up or dispatch anything.
func TestUpdateTask_InProgressNoWorkflowNoOp(t *testing.T) {
	svc, a := setupTaskService(t)

	tk, err := a.tasks.Create("no prior wf", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.UpdateTask(tk.ID, map[string]any{"status": "in-progress"}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress {
		t.Errorf("status = %q, want in-progress", got.Status)
	}
}

func TestUpdateTask_ReadyReviewDispatchesTerminalWorkflowByTrigger(t *testing.T) {
	svc, a := setupTaskService(t)
	tk, err := a.tasks.Create("retry review", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	stale := &workflow.Execution{WorkflowID: "simple-task", State: workflow.ExecFailed}
	if _, err := a.tasks.Update(tk.ID, task.Update{Workflow: &stale}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Update(tk.ID, task.Update{
		Status:          task.Ptr(task.StatusHumanRequired),
		Escalation:      task.OperatorDecisionEvidence("test.fixture_human_required", "test fixture"),
		AutonomyOutcome: task.HumanRequiredOutcome(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.UpdateTask(tk.ID, map[string]any{"status": string(task.StatusReadyReview)}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "simple-task-review" {
		t.Fatalf("workflow = %+v, want simple-task-review selected by ready-review trigger", got.Workflow)
	}
}

func TestUpdateTask_HaltedUnblockToTodoSaysWhatWillDispatch(t *testing.T) {
	// Given a task whose workflow a circuit-breaker trip halted at its
	// testing stage
	svc, a := setupTaskService(t)
	tk, err := a.tasks.Create("halted", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	halted := &workflow.Execution{WorkflowID: "testing-task", CurrentStep: "run_test", State: workflow.ExecFailed}
	if _, err := a.tasks.Update(tk.ID, task.Update{Workflow: &halted}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusBlocked)}); err != nil {
		t.Fatal(err)
	}

	// When the operator unblocks it the obvious way
	if _, err := svc.UpdateTask(tk.ID, map[string]any{"status": string(task.StatusTodo)}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	// Then the task says the status will not dispatch it, and names the one
	// that will, instead of going quiet
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.StatusReason, "testing") {
		t.Fatalf("status reason = %q, want it to name the testing stage", got.StatusReason)
	}
	if !strings.Contains(got.StatusReason, "does not dispatch") {
		t.Fatalf("status reason = %q, want it to say this status dispatches nothing", got.StatusReason)
	}
}

func TestUpdateTask_HaltedUnblockToTheRightStageIsQuiet(t *testing.T) {
	// Given the same halted task
	svc, a := setupTaskService(t)
	tk, err := a.tasks.Create("halted", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	halted := &workflow.Execution{WorkflowID: "testing-task", CurrentStep: "run_test", State: workflow.ExecFailed}
	if _, err := a.tasks.Update(tk.ID, task.Update{Workflow: &halted}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusBlocked)}); err != nil {
		t.Fatal(err)
	}

	// When the operator picks the stage that does dispatch it
	if _, err := svc.UpdateTask(tk.ID, map[string]any{"status": string(task.StatusTesting)}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	// Then nothing warns them off the move that works
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.StatusReason, "does not dispatch") {
		t.Fatalf("status reason = %q, want no warning on the status that resumes it", got.StatusReason)
	}
}
