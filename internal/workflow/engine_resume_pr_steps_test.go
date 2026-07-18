package workflow

import (
	"testing"
	"time"
)

func TestIsResumableStepType(t *testing.T) {
	resumable := []StepType{StepRunAgent, StepParallel, StepBestOfN, StepClassifyTask, StepCreatePR, StepPushBranch, StepPromoteBestOfN}
	for _, st := range resumable {
		if !isResumableStepType(st) {
			t.Errorf("isResumableStepType(%q) = false, want true", st)
		}
	}
	notResumable := []StepType{StepSetStatus, StepCondition, StepShell, StepWaitHuman, StepSyncBranch}
	for _, st := range notResumable {
		if isResumableStepType(st) {
			t.Errorf("isResumableStepType(%q) = true, want false", st)
		}
	}
}

func TestResumeStalled_ResumesParkedCreatePR(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "pr-wf",
		Name: "pr wf",
		Steps: []Step{
			{ID: "create_pr", Name: "Create PR", Type: StepCreatePR, Next: []Transition{{GoTo: ""}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:          "t1",
		Status:      "ready-pr",
		Branch:      "feat/my-branch",
		ProjectID:   "acme/widgets",
		ProjectType: "pet",
		Title:       "feat(x): y",
		Body:        "body",
		Workflow: &Execution{
			WorkflowID:  "pr-wf",
			CurrentStep: "create_pr",
			State:       ExecWaiting,
			Variables: map[string]string{
				workflowRetryAfterVar: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			},
		},
	})

	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	creator := &fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)
	engine.SetPRContentGenerator(&fakePRContentGenerator{title: "feat(x): y", body: "## Motivation\n\nz\n\n## Implementation information\n\nw"})

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 42 {
		t.Fatalf("PRNumber = %d, want 42 — parked create_pr was not resumed (status=%q reason=%q)", ti.PRNumber, ti.Status, tasks.Reason("t1"))
	}
}

func TestResumeStalled_ResumesParkedPromoteBestOfN(t *testing.T) {
	engine, tasks, _, attemptWorktrees, _ := newBestOfNTestEngine(t, 2)

	ti, _ := tasks.GetTask("t1")
	ti.Workflow = &Execution{
		WorkflowID:  "bestofn-test",
		CurrentStep: "promote",
		State:       ExecRunning,
		StepHistory: []StepRecord{
			{StepID: "judge", Status: "completed", Output: `{"winner_attempt_id": "a1"}`},
		},
		BestOfNInflight: map[string]*BestOfNInflight{
			"attempts": {
				ParentStepID: "attempts",
				Attempts: map[string]*AttemptStatus{
					"a1": {AttemptID: "a1", Dir: "/tmp/attempt-a1", Branch: "task-branch-a1", Status: "completed"},
					"a2": {AttemptID: "a2", Dir: "/tmp/attempt-a2", Branch: "task-branch-a2", Status: "completed"},
				},
			},
		},
		Variables: map[string]string{
			workflowRetryAfterVar: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	tasks.Put(ti)

	engine.ResumeStalled()

	ti, _ = tasks.GetTask("t1")
	if ti.Status != "ready-review" {
		t.Fatalf("status = %q, want ready-review — parked promote_best_of_n was not resumed (reason=%q)", ti.Status, tasks.Reason("t1"))
	}
	if len(attemptWorktrees.promoteCalls) != 1 || attemptWorktrees.promoteCalls[0].WinnerDir != "/tmp/attempt-a1" {
		t.Fatalf("promoteCalls = %+v, want single call promoting a1", attemptWorktrees.promoteCalls)
	}
}

func TestResumeStalled_ResumesParkedPushBranch(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")
	local := headSHA(t, wtPath)

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "push-wf",
		Name: "push wf",
		Steps: []Step{
			{ID: "push_existing_pr", Name: "Push Branch", Type: StepPushBranch, Next: []Transition{{GoTo: ""}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "ready-pr",
		Branch:    "feat/my-branch",
		PRNumber:  7,
		ProjectID: "acme/widgets",
		Workflow: &Execution{
			WorkflowID:  "push-wf",
			CurrentStep: "push_existing_pr",
			State:       ExecWaiting,
			Variables: map[string]string{
				workflowRetryAfterVar: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			},
		},
	})

	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRHeadFetcher(&fakePRHeadFetcher{sha: local})

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow state = %v at step %q, want completed — parked push_existing_pr was not resumed (reason=%q)",
			ti.Workflow.State, ti.Workflow.CurrentStep, tasks.Reason("t1"))
	}
}
