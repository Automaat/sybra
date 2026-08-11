package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestPlanReject_ThenApprove(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage → planning path.
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})

	// Plan completes → wait_human.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Reject with feedback.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs more detail"}); err != nil {
		t.Fatal(err)
	}

	// Should go back to plan step. Since reuse_agent=true and the plan agent
	// is still in the roles map, it should send a prompt instead of starting new.
	prompts := agents.SentPrompts()
	if len(prompts) == 0 {
		t.Fatal("expected SendPrompt to be called for reuse_agent")
	}
	if prompts[len(prompts)-1].Message == "" {
		t.Fatal("expected non-empty feedback message")
	}

	// Plan agent completes again → wait_human again.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Now approve.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation after approve, got %q", agents.LastCall().Role)
	}
}

func TestTriageRetry_Success(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage fails twice.
	for range 2 {
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}

	// Should have retried — 3 StartAgent calls total (1 initial + 2 retries).
	triageCalls := 0
	for _, c := range agents.calls {
		if c.Role == "triage" {
			triageCalls++
		}
	}
	if triageCalls != 3 {
		t.Fatalf("expected 3 triage calls, got %d", triageCalls)
	}

	// Third attempt succeeds.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Should advance to set_in_progress → implement.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
}

func TestTriageRetry_Exhausted(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Fail 4 times (initial + 3 retries = 4 total, exceeds max_retries: 3).
	for range 4 {
		agents.SimulateComplete("t1")
		// Ignore errors — last one may fail transition resolution.
		_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"})
	}

	// After exhaustion, the transition should resolve (fallback goto: set_in_progress).
	ti, _ := tasks.GetTask("t1")
	// Workflow should have advanced past triage or failed.
	if ti.Workflow.CurrentStep == "triage" && ti.Workflow.State == ExecRunning {
		t.Fatal("expected workflow to advance past triage after retry exhaustion")
	}
}

func TestPlanningRetry_ExhaustedParksHumanRequired(t *testing.T) {
	store := newInlineTestStore(t, "simple-task-plan", `
id: simple-task-plan
name: test planning retry
trigger:
  on: task.created
steps:
  - id: plan
    type: run_agent
    config:
      role: plan
      max_retries: 1
    next:
      - goto: done
  - id: done
    type: set_status
    config:
      status: todo
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "headless"})
	if err := engine.StartWorkflowFromStepWithVars("t1", "simple-task-plan", "plan", nil); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "failed", Output: "planner crashed"}); err != nil {
			t.Fatal(err)
		}
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "planning plan retry budget exhausted") ||
		!strings.Contains(ti.StatusReason, "planner crashed") {
		t.Fatalf("status_reason = %q, want retry exhaustion with output", ti.StatusReason)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecFailed || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want failed terminal workflow", ti.Workflow)
	}
}

func TestPlanReuse_RejectResetsStatusAndReusesAgentWithFeedback(t *testing.T) {
	engine, tasks, agents := startPlanReuseAtReviewPlan(t)

	// Arrange — the plan agent is still "running" (reuse_agent relies on
	// FindRunningAgentForRole). Record how many SendPrompt calls we've
	// seen so we can assert exactly one more is added by the reject.
	sentBefore := len(agents.SentPrompts())

	// Act — user rejects the plan with free-text feedback. The reject
	// branch routes review_plan → start_replan (set_status planning) →
	// plan, which hits the reuse_agent path.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "add error handling"}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Assert 1 — task status was reset by start_replan, so the next
	// plan-review transition is observable as a real change event.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "planning" {
		t.Errorf("Status = %q, want planning (reset by start_replan)", ti.Status)
	}

	// Assert 2 — the workflow re-entered the plan run_agent step.
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("CurrentStep = %q, want plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Assert 3 — the reused agent received exactly one new prompt
	// carrying the feedback (verbatim, via the rendered template).
	sent := agents.SentPrompts()
	if len(sent) != sentBefore+1 {
		t.Fatalf("SendPrompt count = %d, want %d", len(sent), sentBefore+1)
	}
	msg := sent[len(sent)-1].Message
	if !strings.Contains(msg, "Plan rejected") {
		t.Errorf("prompt missing rejection header: %q", msg)
	}
	if !strings.Contains(msg, "add error handling") {
		t.Errorf("prompt missing feedback: %q", msg)
	}

	// Assert 4 — no new agent was spawned (reuse, not restart).
	if got := agents.CallCount(); got != 1 {
		t.Errorf("StartAgent called %d times, want 1 (reuse only)", got)
	}
}

func TestPlanReuse_RejectThenReplanAdvancesOnStatusChange(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	// Reject — workflow should re-enter plan step waiting for the agent.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs detail"}); err != nil {
		t.Fatal(err)
	}

	// Simulate the plan agent delivering a revised plan and flipping
	// the status back to plan-review.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	// The workflow should be back at review_plan waiting for a fresh
	// human action. Without the set_status reset, the status would
	// already be plan-review when the agent ran and no hook would fire.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestPlanReuse_ApproveAdvancesPastReviewPlan(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("Status = %q, want in-progress (set by done step)", ti.Status)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", ti.Workflow.State)
	}
}

func TestAutoApprovePlanReview_NoOpenDecisionsAdvances(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:            "t1",
		Status:        "planning",
		PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
		PlanContract:  validPlanContract("t1"),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	step := autoApproveReviewStep()
	wf := &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	if err := engine.execWaitHuman("t1", step, wf); err != nil {
		t.Fatal(err)
	}

	waitForTaskStatus(t, tasks, "t1", "in-progress")
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", ti.Workflow.State)
	}
	if got := ti.Workflow.Variables["human.auto_approved"]; got != "true" {
		t.Errorf("human.auto_approved = %q, want true", got)
	}
}

func TestAutoApprovePlanReview_OpenDecisionsStayWaiting(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:     "t1",
		Status: "planning",
		PlanDecisions: "# Decisions\n\n## Scope\nQuestion: Which scope?\nRecommended: Small\n\nOptions:\n" +
			"- Small - Minimal change\n- Large - Broader change",
		PlanContract: validPlanContract("t1"),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
}

func TestAutoApprovePlanReview_InvalidContractStaysWaiting(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:            "t1",
		Status:        "planning",
		PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
		PlanContract:  strings.Replace(validPlanContract("t1"), `"task_id": "t1"`, `"task_id": "other"`, 1),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
}

func TestAutoApprovePlanReview_UnfavorableVerdictStaysWaiting(t *testing.T) {
	cases := []struct {
		name     string
		critique string
	}{
		{
			name:     "REFINE",
			critique: "## Verdict: REFINE\n\nMissing caller file in the file list; compile will break.",
		},
		{
			name:     "REJECT",
			critique: "## Verdict: REJECT\n\nApproach fundamentally unsound; needs a full replan.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
				ID:            "t1",
				Status:        "planning",
				PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
				PlanContract:  validPlanContract("t1"),
				PlanCritique:  tc.critique,
			})
			engine.SetAutoApprovePlansWithoutDecisions(true)

			if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
				WorkflowID:  "simple-task-plan",
				CurrentStep: "review_plan",
				State:       ExecRunning,
				Variables:   map[string]string{},
			}); err != nil {
				t.Fatal(err)
			}

			// A REFINE/REJECT verdict names concrete blockers a human must look
			// at even though the decisions sidecar has nothing left for a human
			// to choose between — "no open decisions" is not the same as "safe
			// to execute as-is". Auto-approve must not paper over that.
			assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
		})
	}
}

func TestPlanReuse_ApproveRepairsMissedWaitForStatus(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	// Simulate a cross-process sybra-cli status write that happened while the
	// app was down, or whose watcher event was missed: the task is visibly
	// plan-review, but the workflow is still parked on the run_agent step.
	tasks.SetStatus("t1", "plan-review")
	stuck, _ := tasks.GetTask("t1")
	if stuck.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: CurrentStep = %q, want plan", stuck.Workflow.CurrentStep)
	}

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("Status = %q, want in-progress", ti.Status)
	}
	if ti.Workflow == nil || ti.Workflow.CurrentStep != "" {
		t.Fatalf("CurrentStep = %v, want completed workflow", ti.Workflow)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want %q", ti.Workflow.State, ExecCompleted)
	}
}

func TestDuplicatePlanAgent_StaleCompletionDoesNotFailWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage runs → agent flips status to planning → advance into plan step.
	triageAgent := agents.LastID()
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: triageAgent, Result: "triaged", Success: true})

	planAgent1 := agents.LastID()
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: current_step = %q, want plan", ti.Workflow.CurrentStep)
	}

	// Inject a duplicate plan agent as if a ResumeStalled ticker fired
	// during the interactive-spawn window and raced the first agent. The
	// workflow route records what execRunAgent would have set.
	agents.mu.Lock()
	agents.counter++
	planAgent2 := fmt.Sprintf("agent-%d", agents.counter)
	agents.calls = append(agents.calls, startCall{TaskID: "t1", Role: "plan", Mode: "interactive"})
	agents.running["t1"] = planAgent2
	agents.roles["t1/plan"] = planAgent2
	agents.mu.Unlock()
	setWorkflowAgentRoute(t, tasks, "t1", planAgent2, "plan")

	// Agent 1 completes first → workflow advances to review_plan/wait_human.
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent1, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("after first plan completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("after first plan completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Agent 2 (the duplicate) finishes seconds later. Old behavior would
	// drive review_plan into ExecFailed. New behavior: dropped as stale.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent2, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("after stale completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("after stale completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}

	// The human's rejection must now succeed — this is the end-to-end
	// symptom the user reported ("task is not waiting for human action").
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "try again"}); err != nil {
		t.Fatalf("HandleHumanAction reject after stale duplicate: %v", err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("after reject: current_step = %q, want plan (loop back)", ti.Workflow.CurrentStep)
	}
}

func TestExecRequireSidecar_PlanCritiquePresent(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# critique\n"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1", PlanCritique: "# critique\n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "present") {
		t.Errorf("Output = %q, want 'present'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("unexpected status flip to human-required")
	}
}

func TestExecRequireSidecar_PlanCritiqueMissingFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (mechanical step)", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(tasks.Reason("t1"), "plan critique") {
		t.Errorf("reason = %q, want substring 'plan critique'", tasks.Reason("t1"))
	}
}

func TestExecRequireSidecar_AllowMissingDoesNotFlipHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarAllowMissingStep("plan_critique"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped warning", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatal("allow_missing should not flip task to human-required")
	}
}

func TestExecRequireSidecar_AllowMissingRejectedOutsidePlanCritique(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	step := newRequireSidecarStep("code_review")
	step.Config.AllowMissing = true
	_, err := engine.execRequireSidecar("t1", step, TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected allow_missing validation error")
	}
	if !strings.Contains(err.Error(), "allow_missing is only supported") {
		t.Fatalf("error = %v, want allow_missing validation", err)
	}
}

func TestExecRequireSidecar_CodeReviewMissingFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("code_review"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(tasks.Reason("t1"), "code review") {
		t.Errorf("reason = %q, want substring 'code review'", tasks.Reason("t1"))
	}
}

func TestExecRequireSidecar_RetriesProducerBeforeHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	wf := &Execution{WorkflowID: "test-simple", CurrentStep: "require", State: ExecRunning, Variables: map[string]string{}}
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Planning, Workflow: wf})
	engine := newEngineForEval(t, tasks)
	step := newRequireSidecarStep("plan")
	step.Config.RetryStep = "plan"
	step.Config.MaxRetries = 2

	for attempt := 1; attempt <= 2; attempt++ {
		current, _ := tasks.GetTask("t1")
		current.Workflow.CurrentStep = "require"
		current.Workflow.State = ExecRunning
		if err := tasks.SetWorkflow("t1", current.Workflow); err != nil {
			t.Fatal(err)
		}
		_, err := engine.execRequireSidecarWithExec("t1", step, current.Workflow, current)
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d err = %v, want errStepParked", attempt, err)
		}
		got, _ := tasks.GetTask("t1")
		if got.Status == taskstatus.HumanRequired || got.Workflow.CurrentStep != "plan" || got.Workflow.State != ExecWaiting {
			t.Fatalf("attempt %d task = %+v, want producer re-armed", attempt, got)
		}
	}

	current, _ := tasks.GetTask("t1")
	current.Workflow.CurrentStep = "require"
	current.Workflow.State = ExecRunning
	if err := tasks.SetWorkflow("t1", current.Workflow); err != nil {
		t.Fatal(err)
	}
	out, err := engine.execRequireSidecarWithExec("t1", step, current.Workflow, current)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Fatalf("exhausted output = %+v", out)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != taskstatus.HumanRequired {
		t.Fatalf("status = %q, want human-required after retry exhaustion", got.Status)
	}
}

func TestExecRequireSidecar_PlanDecisionsPresent(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanDecisions: "# Decisions\n"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_decisions"), TaskInfo{ID: "t1", PlanDecisions: "# Decisions\n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "plan decisions present") {
		t.Errorf("Output = %q, want plan decisions present", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("unexpected status flip to human-required")
	}
}

func TestExecRequireSidecar_WhitespaceOnlyTreatedAsMissing(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1", PlanCritique: "   \n\t  \n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

func TestExecRequireSidecar_UnknownSidecarErrors(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execRequireSidecar("t1", newRequireSidecarStep("bogus"), TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected error for unknown sidecar, got nil")
	}
}

func TestExecRequireSidecar_EmptySidecarConfigErrors(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execRequireSidecar("t1", newRequireSidecarStep(""), TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected error for empty sidecar config, got nil")
	}
}

func TestParsePlanCritiqueVerdict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"approve", "# Plan Review: APPROVE\n\n## Verdict\n\nLooks good.", "APPROVE"},
		{"refine", "# Plan Review: REFINE\n\n## Findings\n\n- missing file", "REFINE"},
		{"reject", "# Plan Review: REJECT\n\nToo vague.", "REJECT"},
		{"lowercase verdict word", "# plan review: refine\n", "REFINE"},
		{"leading blank line", "\n# Plan Review: REFINE\n", "REFINE"},
		{"mention in prose is not the verdict line", "# Plan Review: APPROVE\n\nNo REFINE needed here.", "APPROVE"},
		{"no marker at all", "Looks fine to me.", ""},
		{"empty", "", ""},
		{
			"bare title falls back to Verdict section prose",
			"# Plan Review\n\n## Verdict\n\nThis plan needs REFINE — the rollback step is missing and the test command doesn't match the project.",
			"REFINE",
		},
		{
			"verdict section fallback is bounded to that section",
			"# Plan Review\n\n## Verdict\n\nSound overall.\n\n## Findings\n\n- [nit] refine the error message wording",
			"",
		},
		{
			"inflected verdict word still resolves to its base form",
			"# Plan Review: REJECTED\n\n## Verdict\n\nThis plan is rejected due to missing rollback safety verification steps.",
			"REJECT",
		},
		{
			"heading-level drift on the title line still matches",
			"## Plan Review: REFINE\n\n## Verdict\n\nSeveral steps need adjustment.",
			"REFINE",
		},
		{
			"a longer unrelated word is not a boundary-crossing false match",
			"# Plan Review\n\n## Verdict\n\nNo concerns; this is not a rejectionist take, just a sanity check.",
			"",
		},
		{
			"current skill contract: verdict on the Verdict heading line",
			"# Plan Review\n\n## Verdict: REFINE\n\n**One-line summary:** Missing rollback step.\n\n## Findings\n\n- [high] no rollback",
			"REFINE",
		},
		{
			"current skill contract: APPROVE on the Verdict heading line",
			"# Plan Review\n\n## Verdict: APPROVE\n\n**One-line summary:** Looks executable as-is.",
			"APPROVE",
		},
		{
			"current skill contract: unrendered template brackets still resolve",
			"# Plan Review\n\n## Verdict: [REJECT]\n\n**One-line summary:** Too vague to execute.",
			"REJECT",
		},
		{
			"colon-line format takes priority over a conflicting title line",
			"# Plan Review: APPROVE\n\n## Verdict: REFINE\n\n**One-line summary:** Needs edits despite the stale title.",
			"REFINE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePlanCritiqueVerdict(tt.content); got != tt.want {
				t.Errorf("parsePlanCritiqueVerdict(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestExecFlagPlanCritique_ApproveDoesNotAppendNote(t *testing.T) {
	tasks := newMemTasks()
	wf := &Execution{Variables: map[string]string{planCritiqueVerdictVar: planCritiqueVerdictApprove}}
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: APPROVE\n\nLooks good.", Workflow: wf})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), wf, TaskInfo{ID: "t1", PlanCritique: "# Plan Review: APPROVE\n\nLooks good.", Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if strings.Contains(ti.Body, "Plan Critic Verdict") {
		t.Errorf("APPROVE should not append a verdict note; body = %q", ti.Body)
	}
}

func TestExecFlagPlanCritique_RefineAppendsDistinguishableNote(t *testing.T) {
	tasks := newMemTasks()
	wf := &Execution{Variables: map[string]string{planCritiqueVerdictVar: planCritiqueVerdictRefine}}
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: REFINE\n\n## Findings\n\n- [high] missing file", Workflow: wf})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), wf, TaskInfo{ID: "t1", PlanCritique: "# Plan Review: REFINE\n\n## Findings\n\n- [high] missing file", Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "REFINE") {
		t.Errorf("Output = %q, want it to report the REFINE verdict", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if !strings.Contains(ti.Body, "Plan Critic Verdict: REFINE") {
		t.Errorf("expected a distinguishable REFINE note in body; got:\n%s", ti.Body)
	}
	if ti.Status == "human-required" {
		t.Error("flag_plan_critique must not block progression by itself")
	}
}

func TestExecFlagPlanCritique_RejectAppendsDistinguishableNote(t *testing.T) {
	tasks := newMemTasks()
	wf := &Execution{Variables: map[string]string{planCritiqueVerdictVar: planCritiqueVerdictReject}}
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: REJECT\n\nToo vague.", Workflow: wf})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), wf, TaskInfo{ID: "t1", PlanCritique: "# Plan Review: REJECT\n\nToo vague.", Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if !strings.Contains(ti.Body, "Plan Critic Verdict: REJECT") {
		t.Errorf("expected a distinguishable REJECT note in body; got:\n%s", ti.Body)
	}
}
