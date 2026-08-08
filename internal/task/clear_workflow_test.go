package task

import (
	"encoding/json"
	"testing"

	"github.com/Automaat/sybra/internal/workflow"
)

func TestUpdate_ClearWorkflowSurvivesJSON(t *testing.T) {
	// The old encoding was a non-nil outer pointer holding a nil inner one, which marshals to null; unmarshal then nils the outer pointer, so the server read "no change" and reopen left the dead execution attached.
	in := Update{ClearWorkflow: Ptr(true)}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Update
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ClearWorkflow == nil || !*out.ClearWorkflow {
		t.Fatalf("ClearWorkflow did not survive the round trip: %s", data)
	}
}

func TestApplyUpdate_ClearWorkflowRemovesTheExecution(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	created, err := store.Create("wedged", "", AgentModeHeadless)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Update(created.ID, Update{Workflow: Ptr(&workflow.Execution{WorkflowID: "simple-task-implement", State: workflow.ExecRunning})}); err != nil {
		t.Fatalf("attach workflow: %v", err)
	}
	cleared, err := store.Update(created.ID, Update{ClearWorkflow: Ptr(true)})
	if err != nil {
		t.Fatalf("clear workflow: %v", err)
	}
	if cleared.Workflow != nil {
		t.Errorf("Workflow = %+v, want nil", cleared.Workflow)
	}
}
