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

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "resume-target",
		Name: "resume target",
		Steps: []Step{
			{
				ID:   "triage",
				Name: "Triage",
				Type: StepSetStatus,
				Config: StepConfig{
					Status: "in-progress",
				},
				Next: []Transition{{GoTo: "resume_here"}},
			},
			{
				ID:   "resume_here",
				Name: "Resume Here",
				Type: StepWaitHuman,
				Config: StepConfig{
					HumanActions: []string{"done"},
				},
				Next: []Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "resume_original",
		State:       ExecRunning,
		Variables: map[string]string{
			"resume_workflow_id":   "resume-target",
			"resume_workflow_step": "resume_here",
			"resume_status":        "testing",
			"resume_status_reason": "waiting for branch-conflict recovery",
		},
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress", // set by branch-conflict-fix's own set_recovering step
		StatusReason: "recovering branch conflict",
		Workflow:     wf,
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
	if got.StatusReason != "waiting for branch-conflict recovery" {
		t.Fatalf("status_reason = %q, want restored reason", got.StatusReason)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "resume-target" {
		t.Fatalf("workflow = %+v, want resumed resume-target", got.Workflow)
	}
	if got.Workflow.CurrentStep != "resume_here" {
		t.Fatalf("current step = %q, want captured resume_here step", got.Workflow.CurrentStep)
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
			"resume_status":        "in-progress",
			"resume_status_reason": "keep me",
		},
	}
	tasks.Put(TaskInfo{
		ID:           "t2",
		Status:       "in-progress",
		StatusReason: "recovering branch conflict",
		Workflow:     wf,
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
	if got.StatusReason != "keep me" {
		t.Fatalf("status_reason = %q, want restored reason", got.StatusReason)
	}
	// No workflow was started — the (still-branch-conflict-fix) execution is
	// left for the caller (executeSteps) to finalize via the normal
	// resolveNext path.
	if got.Workflow == nil || got.Workflow.WorkflowID != "branch-conflict-fix" {
		t.Fatalf("workflow = %+v, want unchanged branch-conflict-fix pending resolveNext", got.Workflow)
	}
}
