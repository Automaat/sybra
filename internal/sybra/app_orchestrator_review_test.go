package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestInboundReviewNeedsAgent_CompletedPRReviewIsNotRedispatched is a
// regression test for #2035: reconcileReviewPhases only moves ReviewPhase off
// "" once it observes a live GitHub signal, which can lag well behind (or
// never happen in an offline/test context after) a pr-review workflow
// completing. Treating ReviewPhase=="" as "needs a fresh dispatch" whenever a
// pr-review workflow has already run to completion caused the fast dispatch
// loop to restart pr-review from scratch on every tick — 28 review agent runs
// within seconds in the reported repro. Once pr-review has completed at least
// once, the review already happened; only a task that never ran pr-review
// (nil workflow, or stranded on a foreign workflow like simple-task-plan from
// the create-before-tag race) should report "needs agent".
func TestInboundReviewNeedsAgent_CompletedPRReviewIsNotRedispatched(t *testing.T) {
	completedAt := time.Now().Add(-time.Minute)

	tests := []struct {
		name string
		wf   *workflow.Execution
		want bool
	}{
		{
			name: "never dispatched",
			wf:   nil,
			want: true,
		},
		{
			name: "stranded on foreign terminal workflow",
			wf:   &workflow.Execution{WorkflowID: "simple-task-plan", State: workflow.ExecCompleted, CompletedAt: &completedAt},
			want: true,
		},
		{
			name: "pr-review completed, GitHub signal not yet observed",
			wf:   &workflow.Execution{WorkflowID: "pr-review", State: workflow.ExecCompleted, CompletedAt: &completedAt},
			want: false,
		},
		{
			name: "pr-review failed, GitHub signal not yet observed",
			wf:   &workflow.Execution{WorkflowID: "pr-review", State: workflow.ExecFailed, CompletedAt: &completedAt},
			want: false,
		},
		{
			name: "pr-review still running",
			wf:   &workflow.Execution{WorkflowID: "pr-review", State: workflow.ExecRunning},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tsk := task.Task{
				Tags:        []string{"review"},
				ProjectID:   "owner/repo",
				PRNumber:    7,
				ReviewPhase: "",
				Workflow:    tt.wf,
			}
			if got := inboundReviewNeedsAgent(tsk); got != tt.want {
				t.Errorf("inboundReviewNeedsAgent() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReconcileRunnableBoardTasks_ConvergedInboundReviewIsNoOp pins the
// reported #2035 production shape at the real maintenance loop boundary: a
// review task already converged onto a terminal pr-review workflow, but whose
// ReviewPhase has not caught up from GitHub polling yet, must not start a new
// pr-review workflow on this tick or any later tick. The second tick is the
// acceptance-critical part: it must stay a no-op instead of bouncing the task
// in-review -> in-progress -> in-review forever.
func TestReconcileRunnableBoardTasks_ConvergedInboundReviewIsNoOp(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	svc.workflowEngine.SetTaskClassifier(&taskClassifierAdapter{
		tasks:      a.tasks,
		classifier: fakeTriageClassifier{},
	})
	a.workflowEngine = svc.workflowEngine

	tk, err := a.tasks.Create("Review: converged PR", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().Add(-time.Minute)
	got, err := a.tasks.UpdateMap(tk.ID, map[string]any{
		"status":       string(task.StatusInReview),
		"tags":         []string{"review"},
		"project_id":   "Automaat/sybra",
		"pr_number":    2035,
		"review_phase": "",
		"workflow": &workflow.Execution{
			WorkflowID:  "pr-review",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &completedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !got.StatusChangedAt.After(completedAt) {
		t.Fatalf("test precondition failed: status_changed_at=%s must be after workflow completed_at=%s", got.StatusChangedAt, completedAt)
	}

	a.reconcileRunnableBoardTasks()
	a.wg.Wait()
	afterFirst, err := a.tasks.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("first maintenance tick started %d agents; want 0", launcher.startCalls)
	}
	if afterFirst.Workflow == nil || afterFirst.Workflow.WorkflowID != "pr-review" || afterFirst.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("first maintenance tick mutated converged workflow: %+v", afterFirst.Workflow)
	}
	if afterFirst.Status != task.StatusInReview {
		t.Fatalf("first maintenance tick status = %q, want %q", afterFirst.Status, task.StatusInReview)
	}

	a.reconcileRunnableBoardTasks()
	a.wg.Wait()
	afterSecond, err := a.tasks.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("second maintenance tick started %d agents; want still 0", launcher.startCalls)
	}
	if afterSecond.Workflow == nil || afterSecond.Workflow.WorkflowID != "pr-review" || afterSecond.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("second maintenance tick mutated converged workflow: %+v", afterSecond.Workflow)
	}
	if afterSecond.Status != task.StatusInReview {
		t.Fatalf("second maintenance tick status = %q, want %q", afterSecond.Status, task.StatusInReview)
	}
}
