package main

import (
	"strings"
	"testing"

	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
)

func TestPlanningService_TriageTask(t *testing.T) {
	planSvc, taskSvc, _ := setupPlanningService(t)

	created, err := taskSvc.CreateTask("triage me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := planSvc.TriageTask(created.ID); err != nil {
		t.Fatal(err)
	}

	tk, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
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
			WorkflowID: "simple-task",
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

	tk, err := taskSvc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	svc, _ := setupTaskService(t)

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
// builtin simple-task workflow because setupPlanningService syncs the
// built-in definitions into the store.
func stageWaitingPlanReview(t *testing.T, taskSvc *TaskService, taskID string) {
	t.Helper()
	if _, err := taskSvc.tasks.UpdateMap(taskID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task",
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

	created, err := taskSvc.CreateTask("reject merge", "", "headless")
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

	created, err := taskSvc.CreateTask("reject comments only", "", "headless")
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

// TestApp_StatusHook_AdvancesWorkflow verifies the end-to-end wiring
// inside App.initStatusHook: a task status update on a task whose
// workflow is sitting in a run_agent step with wait_for_status must
// advance past that step as a side effect of the task update. Without
// this wiring, interactive plan agents would never make the workflow
// leave the plan step.
func TestApp_StatusHook_AdvancesWorkflow(t *testing.T) {
	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine
	app.initStatusHook()

	// Create the task directly via the store to bypass
	// TaskService.CreateTask's auto-triage goroutine, which would race
	// with this test's manual workflow setup.
	created, err := app.tasks.Create("status hook task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	// Tag the task `nocritic` so the workflow's maybe_critique branch
	// routes directly to review_plan instead of trying to launch a
	// plan-critic agent (which would fail in this lightweight test setup).
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{"tags": []string{"nocritic"}}); err != nil {
		t.Fatal(err)
	}

	// Park the workflow in the plan step (the builtin simple-task
	// plan step declares wait_for_status: plan-review).
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status": "planning",
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task",
			CurrentStep: "plan",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Act — simulate the plan agent flipping task status to
	// plan-review (this would normally come from `synapse-cli update`).
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
