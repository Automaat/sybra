package workflow

import (
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/worktreeerr"
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

// TestExecResumeWorkflow_SkipsStaleRebaseHumanRequired verifies the #47dcecdd
// fix: when the captured resume_status is human-required with the exact
// rebase-blocked reason, execResumeWorkflow must NOT restore it. Reaching this
// step at all means route_result/verify_commits/detect_tampering all passed,
// so the branch conflict this human-required flip was reporting is already
// resolved — restoring it would immediately re-park a task recovery just
// fixed, with no dispatcher left to pick it back up (resume-stalled/sweep
// logic skips human-required tasks).
func TestExecResumeWorkflow_SkipsStaleRebaseHumanRequired(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "resume_original",
		State:       ExecRunning,
		Variables: map[string]string{
			"resume_status":        "human-required",
			"resume_status_reason": worktreeerr.RebaseBlockedReason,
		},
	}
	tasks.Put(TaskInfo{
		ID:           "t3",
		Status:       "in-progress", // set by branch-conflict-fix's own set_recovering step
		StatusReason: "recovering from a branch conflict (no PR yet)",
		Workflow:     wf,
	})

	out, err := engine.execResumeWorkflow("t3", &Step{ID: "resume_original"}, wf)
	if err != nil {
		t.Fatalf("execResumeWorkflow: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}

	got, gerr := tasks.GetTask("t3")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status == "human-required" {
		t.Fatalf("status = %q, must not restore the stale rebase-blocked human-required park", got.Status)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress (set by set_recovering)", got.Status)
	}
	if got.StatusReason != "recovering from a branch conflict (no PR yet)" {
		t.Fatalf("status_reason = %q, want unchanged", got.StatusReason)
	}
}

// TestExecResumeWorkflow_RestoresUnrelatedHumanRequired verifies that a
// captured human-required status IS restored when its reason is not the
// rebase-blocked reason — e.g. a task already parked for an unrelated cause
// (a cost cap) before an incidental branch conflict interrupted it. Only the
// exact rebase-blocked reason is recognized as "this recovery's own doing".
func TestExecResumeWorkflow_RestoresUnrelatedHumanRequired(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "resume_original",
		State:       ExecRunning,
		Variables: map[string]string{
			"resume_status":        "human-required",
			"resume_status_reason": "agent start blocked: task cumulative cost exceeds agent.max_task_cost_usd",
		},
	}
	tasks.Put(TaskInfo{
		ID:           "t4",
		Status:       "in-progress",
		StatusReason: "recovering from a branch conflict (no PR yet)",
		Workflow:     wf,
	})

	_, err := engine.execResumeWorkflow("t4", &Step{ID: "resume_original"}, wf)
	if err != nil {
		t.Fatalf("execResumeWorkflow: %v", err)
	}

	got, gerr := tasks.GetTask("t4")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want restored human-required", got.Status)
	}
	if got.StatusReason != "agent start blocked: task cumulative cost exceeds agent.max_task_cost_usd" {
		t.Fatalf("status_reason = %q, want restored reason", got.StatusReason)
	}
}
