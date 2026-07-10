package workflow

import (
	"errors"
	"testing"
)

// TestExecPushBranch_DivergedRecovery_NestedStartWorkflowFails documents the
// bug behind local task e8bb0d98: push_branch queues divergence recovery until
// StartWorkflowWithVars/DispatchEvent releases its per-task markers, then
// invokes the callback while the original workflow is still non-terminal. A
// callback that reacts to the divergence by calling the ordinary
// StartWorkflowWithVars — the naive "CancelWorkflow then Start" pattern —
// therefore fails with ErrWorkflowAlreadyActive because the task already has
// an active workflow attached, even though nothing else is concurrently
// touching it. Callers must use ReplaceWorkflow instead so the recovery
// workflow takes over atomically (see
// TestExecPushBranch_DivergedRecovery_ReplaceWorkflowSucceeds).
func TestExecPushBranch_DivergedRecovery_NestedStartWorkflowFails(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")
	commitFile(t, wtPath, "one.txt", "one")
	commitFile(t, wtPath, "two.txt", "two")
	runGit(t, wtPath, "push", "-u", "origin", "feat/existing-pr")
	runGit(t, wtPath, "reset", "--hard", "HEAD~1")
	commitFile(t, wtPath, "two-prime.txt", "two-prime")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr"})
	store := newTestStoreWith(t, "test-push-first.yaml", "test-recovery-target.yaml")
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})

	var nestedErr error
	engine.SetDivergenceRecovery(func(taskID string) bool {
		nestedErr = engine.StartWorkflowWithVars(taskID, "test-recovery-target", nil)
		return nestedErr == nil
	})

	if err := engine.StartWorkflowWithVars("t1", "test-push-first", nil); err != nil {
		t.Fatalf("StartWorkflowWithVars(outer): %v", err)
	}
	if !errors.Is(nestedErr, ErrWorkflowAlreadyActive) {
		t.Fatalf("nested StartWorkflowWithVars err = %v, want ErrWorkflowAlreadyActive", nestedErr)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("task status = %q, want human-required (recovery callback reported failure)", ti.Status)
	}
}

// TestExecPushBranch_DivergedRecovery_ReplaceWorkflowSucceeds is the fix:
// the same reentrant callback, using ReplaceWorkflow instead of Cancel+Start,
// succeeds and swaps in the recovery workflow.
func TestExecPushBranch_DivergedRecovery_ReplaceWorkflowSucceeds(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")
	commitFile(t, wtPath, "one.txt", "one")
	commitFile(t, wtPath, "two.txt", "two")
	runGit(t, wtPath, "push", "-u", "origin", "feat/existing-pr")
	runGit(t, wtPath, "reset", "--hard", "HEAD~1")
	commitFile(t, wtPath, "two-prime.txt", "two-prime")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr"})
	store := newTestStoreWith(t, "test-push-first.yaml", "test-recovery-target.yaml")
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})

	var replaceErr error
	engine.SetDivergenceRecovery(func(taskID string) bool {
		replaceErr = engine.ReplaceWorkflow(taskID, "branch conflict recovery", "test-recovery-target", nil)
		return replaceErr == nil
	})

	if err := engine.StartWorkflowWithVars("t1", "test-push-first", nil); err != nil {
		t.Fatalf("StartWorkflowWithVars(outer): %v", err)
	}
	if replaceErr != nil {
		t.Fatalf("ReplaceWorkflow err = %v, want nil", replaceErr)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatalf("task status = %q, want recovery workflow to have taken over", ti.Status)
	}
	if ti.Workflow == nil || ti.Workflow.WorkflowID != "test-recovery-target" {
		t.Fatalf("workflow = %+v, want test-recovery-target", ti.Workflow)
	}
}
