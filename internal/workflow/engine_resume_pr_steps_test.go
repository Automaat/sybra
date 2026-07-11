package workflow

import (
	"testing"
	"time"
)

func TestIsResumableStepType(t *testing.T) {
	resumable := []StepType{StepRunAgent, StepParallel, StepBestOfN, StepClassifyTask, StepCreatePR, StepPushBranch}
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
