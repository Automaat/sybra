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
