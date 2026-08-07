package workflow

import "testing"

// TestFinishTerminalStepOutput_PersistFailureLeavesTaskUnchanged is the
// permutation-harness case for #2749's sharpest crash window: before
// SetStatusAndWorkflow, finishTerminalStepOutput persisted terminal status
// and the completed Execution as two separate store writes, so a crash (or
// any failure) between them could land a `done` task with a still-`running`
// workflow. Simulating that failure point now hits a single store call, so
// either both fields land together or neither does — the board can never
// observe the split state.
func TestFinishTerminalStepOutput_PersistFailureLeavesTaskUnchanged(t *testing.T) {
	tasks := newMemTasks()
	wf := &Execution{WorkflowID: "test-simple", State: ExecRunning, CurrentStep: "step1", Variables: map[string]string{}}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	tasks.failSetWorkflow = true
	clone := wf.Clone()
	if clone == nil {
		t.Fatal("Clone returned nil")
	}
	err := engine.finishTerminalStepOutput("t1", clone, StepOutput{
		StepID:         "step1",
		Status:         "completed",
		TerminalStatus: "done",
		TerminalReason: "finished",
	}, func() {})
	if err == nil {
		t.Fatal("finishTerminalStepOutput: want simulated persist error, got nil")
	}

	got := tasks.mustGetTask(t, "t1")
	if got.Status != "in-progress" {
		t.Fatalf("Status = %q after failed persist, want unchanged in-progress (no partial write)", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != ExecRunning {
		t.Fatalf("Workflow.State = %v after failed persist, want unchanged running (no partial write)", got.Workflow)
	}

	tasks.failSetWorkflow = false
	clone = wf.Clone()
	if clone == nil {
		t.Fatal("Clone returned nil")
	}
	if err := engine.finishTerminalStepOutput("t1", clone, StepOutput{
		StepID:         "step1",
		Status:         "completed",
		TerminalStatus: "done",
		TerminalReason: "finished",
	}, func() {}); err != nil {
		t.Fatalf("finishTerminalStepOutput: %v", err)
	}

	got = tasks.mustGetTask(t, "t1")
	if got.Status != "done" {
		t.Fatalf("Status = %q after successful persist, want done", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted {
		t.Fatalf("Workflow.State = %v after successful persist, want completed", got.Workflow)
	}
}
