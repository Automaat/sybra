package sybra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// waitForWorkflow polls until the task has a non-nil workflow attached or
// the deadline expires. CreateTask auto-spawns a workflow in a goroutine,
// and PlanTask/TriageTask are now idempotent (return nil if a concurrent
// start is already in progress). Either path eventually sets the workflow,
// so tests must wait for the observable end state rather than rely on
// synchronous completion of any single call.
func waitForWorkflow(t *testing.T, taskSvc *TaskService, id string) task.Task {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := taskSvc.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Workflow != nil {
			return tk
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("workflow never became attached to task")
	return task.Task{}
}

func TestPlanningService_TriageTask(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("triage me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := planSvc.TriageTask(created.ID); err != nil {
		t.Fatal(err)
	}

	tk := waitForWorkflow(t, taskSvc, created.ID)
	if tk.Workflow == nil {
		t.Fatal("expected workflow to be set after TriageTask")
	}
}

func TestPlanningService_PlanTask_NoopWithActiveWorkflow(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("plan me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Set up a workflow manually.
	if _, err := taskSvc.tasks.UpdateMap(created.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID: "simple-task-plan",
			State:      workflow.ExecRunning,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// PlanTask should be a no-op.
	if err := planSvc.PlanTask(created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningService_PlanTask_StartsWorkflow(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("plan me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := planSvc.PlanTask(created.ID); err != nil {
		t.Fatal(err)
	}

	tk := waitForWorkflow(t, taskSvc, created.ID)
	if tk.Workflow == nil {
		t.Fatal("expected workflow to be set after PlanTask")
	}
}

func TestPlanningService_ApprovePlan_ErrorWhenNotWaiting(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("approve me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// No workflow running → approve should fail.
	if _, err := planSvc.ApprovePlan(created.ID); err == nil {
		t.Fatal("expected error when approving task without active workflow")
	}
}

func TestPlanningService_ApprovePlan_RecoversCompletedPlanReviewWorkflow(t *testing.T) {
	planSvc, _, a := setupPlanningService(t)

	created, err := a.tasks.Create("approve stale plan", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusPlanReview
	completedAt := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
		Variables: map[string]string{
			"human_action":            "reject",
			"human.feedback":          "stale feedback",
			"step.review_plan.output": "reject",
			"step.critique_plan.note": "keep",
		},
		StepHistory: []workflow.StepRecord{{
			StepID: "validate_plan_contract",
			Status: "completed",
		}},
		StartedAt:   completedAt.Add(-time.Minute),
		CompletedAt: &completedAt,
	}
	plan := "# Execution Plan\n\n## Steps\n1. Fix it."
	planContract := validPlanningContract(created.ID)
	planResearch := "researched relevant files"
	planDecisions := "# Decisions\n\nNo open decisions."
	planBrief := "safe to approve"
	if _, err := a.tasks.Update(created.ID, task.Update{
		Status:        &status,
		Workflow:      &wf,
		Plan:          &plan,
		PlanContract:  &planContract,
		PlanResearch:  &planResearch,
		PlanDecisions: &planDecisions,
		PlanBrief:     &planBrief,
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := planSvc.ApprovePlan(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInProgress {
		t.Fatalf("Status = %q, want %q", updated.Status, task.StatusInProgress)
	}
	if updated.Workflow == nil || updated.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", updated.Workflow)
	}
	if updated.Workflow.CurrentStep != "" {
		t.Fatalf("CurrentStep = %q, want empty after approval", updated.Workflow.CurrentStep)
	}
	if _, ok := updated.Workflow.Variables["human.feedback"]; ok {
		t.Fatal("stale human.feedback should be cleared during recovery")
	}
	if got := updated.Workflow.Variables["step.critique_plan.note"]; got != "keep" {
		t.Fatalf("step variable = %q, want preserved", got)
	}
}

func TestPlanningService_ApprovePlan_DoesNotRecoverWithoutValidatedPlan(t *testing.T) {
	planSvc, _, a := setupPlanningService(t)

	created, err := a.tasks.Create("approve invalid stale plan", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusPlanReview
	completedAt := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
		StartedAt:   completedAt.Add(-time.Minute),
		CompletedAt: &completedAt,
	}
	if _, err := a.tasks.Update(created.ID, task.Update{Status: &status, Workflow: &wf}); err != nil {
		t.Fatal(err)
	}

	if _, err := planSvc.ApprovePlan(created.ID); err == nil {
		t.Fatal("expected error when recovering without validated plan artifacts")
	}
	updated, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusPlanReview {
		t.Fatalf("Status = %q, want %q", updated.Status, task.StatusPlanReview)
	}
	if updated.Workflow == nil || updated.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", updated.Workflow)
	}
}

func TestPlanningService_ApprovePlan_RecoversMarkdownOnlyMigrationPlan(t *testing.T) {
	planSvc, _, a := setupPlanningService(t)

	created, err := a.tasks.Create("approve markdown-only stale plan", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusPlanReview
	completedAt := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
		StepHistory: []workflow.StepRecord{{
			StepID: "validate_plan_contract",
			Status: "completed",
		}},
		StartedAt:   completedAt.Add(-time.Minute),
		CompletedAt: &completedAt,
	}
	plan := "# Execution Plan\n\n## Steps\n1. Fix it."
	planResearch := "researched relevant files"
	planDecisions := "# Decisions\n\nNo open decisions."
	planBrief := "safe to approve"
	if _, err := a.tasks.Update(created.ID, task.Update{
		Status:        &status,
		Workflow:      &wf,
		Plan:          &plan,
		PlanResearch:  &planResearch,
		PlanDecisions: &planDecisions,
		PlanBrief:     &planBrief,
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := planSvc.ApprovePlan(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInProgress {
		t.Fatalf("Status = %q, want %q", updated.Status, task.StatusInProgress)
	}
}

func TestPlanningService_ApprovePlan_RecoversManualVerificationContract(t *testing.T) {
	planSvc, _, a := setupPlanningService(t)

	created, err := a.tasks.Create("approve manual verification plan", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusPlanReview
	completedAt := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
		StepHistory: []workflow.StepRecord{{
			StepID: "validate_plan_contract",
			Status: "completed",
		}},
		StartedAt:   completedAt.Add(-time.Minute),
		CompletedAt: &completedAt,
	}
	plan := "# Execution Plan\n\n## Steps\n1. Fix it."
	planContract := strings.Replace(validPlanningContract(created.ID),
		`{"command": "go test ./internal/sybra", "expected": "tests pass"}`,
		`{"manual": "Inspect the rendered UI.", "expected": "The expected controls are visible."}`, 1)
	planContract = strings.Replace(planContract,
		`  "risk_tier": "low",`,
		`  "ui_constraints": {"preserve_raw_columns": true},
  "stop_conditions": ["Generated bindings require manual edits."],
  "risk_tier": "low",`, 1)
	planResearch := "researched relevant files"
	planDecisions := "# Decisions\n\nNo open decisions."
	planBrief := "safe to approve"
	if _, err := a.tasks.Update(created.ID, task.Update{
		Status:        &status,
		Workflow:      &wf,
		Plan:          &plan,
		PlanContract:  &planContract,
		PlanResearch:  &planResearch,
		PlanDecisions: &planDecisions,
		PlanBrief:     &planBrief,
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := planSvc.ApprovePlan(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInProgress {
		t.Fatalf("Status = %q, want %q", updated.Status, task.StatusInProgress)
	}
}

func TestPlanningService_RejectPlan_ErrorWhenNotWaiting(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("reject me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := planSvc.RejectPlan(created.ID, "bad plan"); err == nil {
		t.Fatal("expected error when rejecting task without active workflow")
	}
}

func validPlanningContract(taskID string) string {
	return fmt.Sprintf(`{
  "task_id": %q,
  "branch": "example-%s",
  "worktree": "/tmp/sybra-%s",
  "files": [
    {"path": "internal/sybra/svc_planning.go", "purpose": "edit", "symbols": ["PlanningService"]}
  ],
  "steps": ["Recover stale plan-review wait state"],
  "verification": [
    {"command": "go test ./internal/sybra", "expected": "tests pass"}
  ],
  "acceptance_criteria": ["Accept Plan works for stale completed plan-review workflows"],
  "risk_tier": "low",
  "permission_tier": "repo-write",
  "rollback": "revert the planning service recovery change"
}`, taskID, taskID, taskID)
}

func TestPlanningService_SendPlanMessage_EmptyMessage(t *testing.T) {
	planSvc, _, _ := setupPlanningService(t)

	err := planSvc.SendPlanMessage("any-id", "")
	if err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestPlanningService_SendPlanMessage_NoAgent(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("msg task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	err = planSvc.SendPlanMessage(created.ID, "hello")
	if err == nil {
		t.Fatal("expected error when no plan agent running")
	}
}

func TestPlanningService_HasLivePlanAgent_FalseByDefault(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("check agent", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if planSvc.HasLivePlanAgent(created.ID) {
		t.Fatal("expected no live plan agent by default")
	}
}

func TestTaskService_CreateTask_AutoStartsWorkflow(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("auto-workflow task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// The workflow auto-start runs in a goroutine, so the task may not
	// have a workflow immediately. Check it was created with todo status.
	if created.Status != task.StatusTodo {
		t.Fatalf("expected todo, got %q", created.Status)
	}
}

func TestTaskService_UpdateTask_CleansWorktreeOnDone(t *testing.T) {
	a := setupApp(t)
	var wg sync.WaitGroup
	svc := &TaskService{
		tasks:     a.tasks,
		agents:    a.agents,
		worktrees: a.worktrees,
		wg:        &wg,
		logger:    a.logger,
	}

	created, err := svc.CreateTask("cleanup task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.UpdateTask(created.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusDone {
		t.Fatalf("expected done, got %q", updated.Status)
	}
}

// --- assembleFeedback: merges free text + unresolved inline comments ---
//
// Both RejectPlan and SendPlanMessage must include unresolved inline
// comments automatically — the plan-review UI placeholder promises this
// behaviour. These tests pin down the formatting so the two paths
// cannot drift.

func TestPlanningService_AssembleFeedback(t *testing.T) {
	tests := []struct {
		name     string
		feedback string
		// comments lists (line, body, resolved?) tuples — nil means no
		// sidecar file exists.
		comments []planCommentSpec
		want     string
	}{
		{
			name:     "empty inputs produce empty string",
			feedback: "",
			comments: nil,
			want:     "",
		},
		{
			name:     "free text only",
			feedback: "needs error handling",
			comments: nil,
			want:     "needs error handling",
		},
		{
			name:     "whitespace-only feedback is trimmed to empty",
			feedback: "   \n  ",
			comments: nil,
			want:     "",
		},
		{
			name:     "unresolved comments only",
			feedback: "",
			comments: []planCommentSpec{
				{line: 5, body: "this step is unclear"},
			},
			want: "Unresolved review comments:\n- Line 5: this step is unclear",
		},
		{
			name:     "feedback plus unresolved comments",
			feedback: "bigger picture: revise the data model",
			comments: []planCommentSpec{
				{line: 2, body: "rename this field"},
				{line: 9, body: "add a test for overflow"},
			},
			want: "bigger picture: revise the data model\n\nUnresolved review comments:\n- Line 2: rename this field\n- Line 9: add a test for overflow",
		},
		{
			name:     "resolved comments are excluded",
			feedback: "also: naming",
			comments: []planCommentSpec{
				{line: 1, body: "already fixed", resolved: true},
				{line: 3, body: "still open"},
			},
			want: "also: naming\n\nUnresolved review comments:\n- Line 3: still open",
		},
		{
			name:     "all comments resolved → free text only",
			feedback: "looks good otherwise",
			comments: []planCommentSpec{
				{line: 1, body: "done", resolved: true},
			},
			want: "looks good otherwise",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planSvc, taskSvc, _ := setupPlanningService(t)
			created, err := taskSvc.CreateTask("feedback test", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range tt.comments {
				added, err := planSvc.tasks.Comments().Add(created.ID, c.line, c.body)
				if err != nil {
					t.Fatal(err)
				}
				if c.resolved {
					if err := planSvc.tasks.Comments().Resolve(created.ID, added.ID); err != nil {
						t.Fatal(err)
					}
				}
			}

			got := planSvc.assembleFeedback(created.ID, tt.feedback)

			if got != tt.want {
				t.Errorf("assembleFeedback =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

type planCommentSpec struct {
	line     int
	body     string
	resolved bool
}

// --- RejectPlan + SendPlanMessage carry merged feedback to the workflow ---

// stageWaitingPlanReview manually drops the task's workflow into an
// ExecWaiting state at a human-action step so HandleHumanAction accepts
// the action without going through the full agent pipeline. Uses the
// builtin simple-task-plan workflow because setupPlanningService syncs
// the built-in definitions into the store.
func stageWaitingPlanReview(t *testing.T, taskSvc *TaskService, taskID string) {
	t.Helper()
	if _, err := taskSvc.tasks.UpdateMap(taskID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "review_plan",
			State:       workflow.ExecWaiting,
			// Seed _dir so the plan step the engine resumes after reject has a
			// valid working directory (the Manager.Run guard requires one).
			Variables: map[string]string{workflow.WorkflowVarDir: t.TempDir()},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningService_RejectPlan_StoresMergedFeedbackInVars(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	// Use tasks.Create directly to avoid the auto-workflow goroutine spawned
	// by CreateTask racing with stageWaitingPlanReview's direct state write.
	created, err := taskSvc.tasks.Create("reject merge", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planSvc.tasks.Comments().Add(created.ID, 4, "mention the error case"); err != nil {
		t.Fatal(err)
	}
	stageWaitingPlanReview(t, taskSvc, created.ID)

	if _, err := planSvc.RejectPlan(created.ID, "top level feedback"); err != nil {
		t.Fatalf("RejectPlan: %v", err)
	}

	updated, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := updated.Workflow.Variables["human.feedback"]
	if !strings.Contains(got, "top level feedback") {
		t.Errorf("human.feedback missing free-text portion: %q", got)
	}
	if !strings.Contains(got, "- Line 4: mention the error case") {
		t.Errorf("human.feedback missing inline comment: %q", got)
	}
	if updated.Workflow.Variables["human_action"] != "reject" {
		t.Errorf("human_action = %q, want reject", updated.Workflow.Variables["human_action"])
	}
}

func TestPlanningService_RejectPlan_WithOnlyUnresolvedCommentsStillStoresFeedback(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	// Use tasks.Create directly to avoid the auto-workflow goroutine spawned
	// by CreateTask racing with stageWaitingPlanReview's direct state write.
	created, err := taskSvc.tasks.Create("reject comments only", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planSvc.tasks.Comments().Add(created.ID, 1, "line 1 problem"); err != nil {
		t.Fatal(err)
	}
	stageWaitingPlanReview(t, taskSvc, created.ID)

	if _, err := planSvc.RejectPlan(created.ID, ""); err != nil {
		t.Fatalf("RejectPlan: %v", err)
	}

	updated, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := updated.Workflow.Variables["human.feedback"]
	if !strings.Contains(got, "Unresolved review comments") {
		t.Errorf("expected comments section in feedback, got %q", got)
	}
	if !strings.Contains(got, "line 1 problem") {
		t.Errorf("expected comment body in feedback, got %q", got)
	}
}

func TestPlanningService_SendPlanMessage_EmptyMessageAndEmptyCommentsRejected(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("send empty", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	err = planSvc.SendPlanMessage(created.ID, "")

	if err == nil {
		t.Fatal("expected error when message and comments are both empty")
	}
}

func TestPlanningService_SendPlanMessage_WithCommentsButNoLiveAgentErrors(t *testing.T) {
	// When there's no live plan agent, SendPlanMessage must surface
	// that clearly instead of silently swallowing the feedback. This
	// also exercises the "comments alone count as a message" branch.
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("send no agent", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planSvc.tasks.Comments().Add(created.ID, 1, "open question"); err != nil {
		t.Fatal(err)
	}

	err = planSvc.SendPlanMessage(created.ID, "")

	if err == nil {
		t.Fatal("expected error about missing plan agent")
	}
	if !strings.Contains(err.Error(), "no live") {
		t.Errorf("error = %v, expected to mention missing plan agent", err)
	}
}

// waitForStatusWorkflowYAML backs TestApp_StatusHook_AdvancesWorkflow. No
// builtin declares wait_for_status since #2152 removed address_critique, but
// the engine feature stays live for user-authored workflows in
// ~/.sybra/workflows, so the App-level hook still needs coverage and can no
// longer borrow a builtin step to park on.
const waitForStatusWorkflowYAML = `id: test-status-hook
name: Test Status Hook
description: Minimal workflow parking a run_agent step on wait_for_status
trigger:
  on: task.created
steps:
  - id: plan
    name: Plan
    type: run_agent
    config:
      role: plan
      mode: interactive
      model: opus
      wait_for_status: plan-review
      prompt: Plan {{.Task.ID}}
    next:
      - goto: review_plan

  - id: review_plan
    name: Review Plan
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
    next:
      - goto: ""
`

// TestApp_StatusHook_AdvancesWorkflow verifies the end-to-end wiring
// inside App.initStatusHook: a task status update on a task whose
// workflow is sitting in a run_agent step with wait_for_status must
// advance past that step as a side effect of the task update. Without
// this wiring, such a workflow would never leave its waiting step.
func TestApp_StatusHook_AdvancesWorkflow(t *testing.T) {
	_, app := setupTaskService(t)

	// setupTaskService syncs only builtins, none of which declare
	// wait_for_status — hence a purpose-built fixture store here.
	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "test-status-hook.yaml"), []byte(waitForStatusWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: app.tasks},
		&agentAdapter{agents: app.agents, agentOrch: app.agentOrch, tasks: app.tasks},
		app.logger,
	)
	engine.SetContext(t.Context())
	app.workflowEngine = engine
	app.initStatusHook()

	// Direct store write, not TaskService.CreateTask: the latter's auto-triage
	// goroutine would race this test's manual workflow setup.
	created, err := app.tasks.Create("status hook task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status":         "planning",
		"plan":           "plan",
		"plan_critique":  "critique",
		"plan_research":  "research",
		"plan_decisions": "decisions",
		"plan_brief":     "brief",
		"workflow": &workflow.Execution{
			WorkflowID:  "test-status-hook",
			CurrentStep: "plan",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Act — simulate the agent flipping task status to plan-review
	// (this would normally come from `sybra-cli update`).
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{"status": "plan-review"}); err != nil {
		t.Fatal(err)
	}

	// Assert — status hook should have called HandleStatusChange,
	// which should have advanced the workflow to review_plan.
	updated, err := app.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workflow == nil {
		t.Fatal("workflow missing after status hook")
	}
	if updated.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan (status hook should advance)", updated.Workflow.CurrentStep)
	}
}
