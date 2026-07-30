package agentorch

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestRevertToTodoAfterGateBlock_RevertsWhenStillInProgress pins the common
// case: a start attempt was blocked before any agent process ran, and the
// task is still sitting in the in-progress row the caller put it in right
// before the blocked start — the revert must land.
func TestRevertToTodoAfterGateBlock_RevertsWhenStillInProgress(t *testing.T) {
	t.Parallel()
	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("gated task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{tasks: tasks, logger: discardSlogLogger()}
	o.revertToTodoAfterGateBlock(tk.ID, "task.revert-on-gate")

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusTodo)
	}
}

// TestRevertToTodoAfterGateBlock_NoOpWhenTaskAlreadyMoved pins the
// transition API's CAS guarantee end to end: if the task is no longer
// in-progress by the time the revert runs (a concurrent writer already
// decided something else — e.g. a human parked it human-required), the
// blind "flip back to todo" must not clobber that newer decision.
func TestRevertToTodoAfterGateBlock_NoOpWhenTaskAlreadyMoved(t *testing.T) {
	t.Parallel()
	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("gated task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("parked by someone else"),
	}); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{tasks: tasks, logger: discardSlogLogger()}
	o.revertToTodoAfterGateBlock(tk.ID, "task.revert-on-gate")

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want unchanged %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != "parked by someone else" {
		t.Fatalf("status reason = %q, want preserved", got.StatusReason)
	}
}
