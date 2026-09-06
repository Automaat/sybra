package workflow

import "testing"

func TestCIAcceptedReviewKeepsInputsAfterRouteWriteFailure(t *testing.T) {
	store := newInlineTestStore(t, "review-inputs", `id: review-inputs
name: review-inputs
steps:
  - id: review
    type: run_agent
    config:
      role: review
      mode: headless
      prompt: Review this change
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	e := NewTestEngine(store, tasks, agents, discardLogger())
	enableTestCI(e)
	wt := makeGitRepo(t, true)
	e.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	rec := newFakeEvidenceRecorder()
	e.SetEvidenceRecorder(rec)
	task := TaskInfo{ID: "t1", Status: "ready-review", AgentMode: "headless", Body: "Implement original contract"}
	tasks.Put(task)
	done := make(chan error, 1)
	go func() { done <- e.StartWorkflow(task.ID, "review-inputs") }()
	<-agents.startEntered
	tasks.failSetWorkflowN = 1
	close(agents.startGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.HandleAgentComplete(task.ID, AgentCompletion{AgentID: "agent-1", Success: true, Result: `{"verdict":"CLEAN"}`})
	ce, err := rec.Evidence(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := ce.ByCriterion(evidenceCriterionReview)
	if !ok {
		t.Fatalf("review did not record evidence: %+v", ce)
	}
	if entry.ContractDigest != e.verificationContractDigest(task) {
		t.Fatalf("accepted review lost dispatch contract after route write failure: %+v", entry)
	}
}
