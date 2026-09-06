package workflow

import (
	"testing"
	"time"
)

func TestReviewProgressBindsOnlyOneReviewExecution(t *testing.T) {
	e := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	task := TaskInfo{ID: "task-review", ProjectID: "repo", Title: "contract", Body: "requirements"}
	wf := &Execution{WorkflowID: "review", StartedAt: time.Now(), DefinitionHash: "definition"}
	step := &Step{ID: "code-review", Type: StepRunAgent, Config: StepConfig{Role: "review", ReviewProgressBase: "refs/remotes/origin/main"}}
	var one, retry AgentAssignment
	e.bindReviewProgress(step, wf, task, &one)
	e.bindReviewProgress(step, wf, task, &retry)
	if one.ReviewLineage == "" || one.ReviewLineage != retry.ReviewLineage || one.ReviewContractDigest != retry.ReviewContractDigest {
		t.Fatal("same execution retry lost stable identity")
	}
	task.Body = "changed requirement"
	e.bindReviewProgress(step, wf, task, &retry)
	if one.ReviewContractDigest == retry.ReviewContractDigest {
		t.Fatal("changed task contract reused notes")
	}
	wf.StartedAt = wf.StartedAt.Add(time.Second)
	e.bindReviewProgress(step, wf, task, &retry)
	if one.ReviewLineage == retry.ReviewLineage {
		t.Fatal("independent review inherited lineage")
	}
	step.Config.Role = "implementation"
	retry = AgentAssignment{}
	e.bindReviewProgress(step, wf, task, &retry)
	if retry.ReviewLineage != "" {
		t.Fatal("implementation inherited review progress")
	}
	step.Config.Role = "review"
	step.Config.ReviewProgressBase = ""
	e.bindReviewProgress(step, wf, task, &retry)
	if retry.ReviewLineage != "" {
		t.Fatal("unbound comparison opted in")
	}
}
