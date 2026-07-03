package workflow

import (
	"errors"
	"testing"
)

// TestExecResumeWorkflow_ResumesCapturedTarget verifies that when a resume
// target was captured (resume_workflow_id set), execResumeWorkflow restores
// the task's pre-recovery status and re-enters that exact workflow directly
// — this is how branch-conflict recovery resumes the interrupted
// review/testing stage instead of skipping to a terminal status.
func TestExecResumeWorkflow_ResumesCapturedTarget(t *testing.T) {
	t.Parallel()

	store := newTestStore(t) // seeds "test-simple" workflow
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "resume_original",
		State:       ExecRunning,
		Variables: map[string]string{
			"resume_workflow_id": "test-simple",
			"resume_status":      "testing",
		},
	}
	tasks.Put(TaskInfo{
		ID:       "t1",
		Status:   "in-progress", // set by branch-conflict-fix's own set_recovering step
		Workflow: wf,
	})

	_, err := engine.execResumeWorkflow("t1", &Step{ID: "resume_original"}, wf)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("execResumeWorkflow error = %v, want errStepParked", err)
	}

	got, gerr := tasks.GetTask("t1")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != "testing" {
		t.Fatalf("status = %q, want restored %q", got.Status, "testing")
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "test-simple" {
		t.Fatalf("workflow = %+v, want resumed test-simple", got.Workflow)
	}
	// test-simple's first step (triage) is a run_agent step — an async step,
	// so the resumed workflow should be parked there waiting for the agent.
	if got.Workflow.CurrentStep != "triage" {
		t.Fatalf("current step = %q, want triage (test-simple's first step)", got.Workflow.CurrentStep)
	}
}

// TestExecResumeWorkflow_NoTargetCompletesNormally verifies the no-op case:
// when no resume_workflow_id was captured (the task had no active workflow
// when recovery began), execResumeWorkflow only restores status and
// completes normally so the recovery workflow ends via the ordinary
// resolveNext path — leaving the status-driven cascade
// (AgentCompletionHandler.OnWorkflowComplete in package sybra) to pick up
// whatever workflow matches the restored status.
func TestExecResumeWorkflow_NoTargetCompletesNormally(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "resume_original",
		State:       ExecRunning,
		Variables: map[string]string{
			"resume_status": "in-progress",
		},
	}
	tasks.Put(TaskInfo{
		ID:       "t2",
		Status:   "in-progress",
		Workflow: wf,
	})

	out, err := engine.execResumeWorkflow("t2", &Step{ID: "resume_original"}, wf)
	if err != nil {
		t.Fatalf("execResumeWorkflow: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}

	got, gerr := tasks.GetTask("t2")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want restored in-progress", got.Status)
	}
	// No workflow was started — the (still-branch-conflict-fix) execution is
	// left for the caller (executeSteps) to finalize via the normal
	// resolveNext path.
	if got.Workflow == nil || got.Workflow.WorkflowID != "branch-conflict-fix" {
		t.Fatalf("workflow = %+v, want unchanged branch-conflict-fix pending resolveNext", got.Workflow)
	}
}
