package task

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestBoardProjectionKeepsCardDataAndDropsExecutionHistory(t *testing.T) {
	task := Task{
		ID: "task", Body: "searchable description",
		AgentRuns: []AgentRun{{
			AgentID: "agent", Role: "review", State: "stopped", StartedAt: time.Now().UTC(), CostUSD: 1.25,
			Provider: providerid.Claude, Model: "large", Prompt: "huge prompt", Result: "huge result", LogFile: "/tmp/log",
		}},
		Attachments: []Attachment{{FileName: "large.bin"}},
		Workflow: &workflow.Execution{
			WorkflowID: "workflow", CurrentStep: "verify_checks", State: workflow.ExecRunning,
			Variables:   map[string]string{"large": "payload"},
			StepHistory: []workflow.StepRecord{{StepID: "implement", Output: "large output"}},
			EffectLog:   []workflow.EffectRecord{{Owner: "old"}},
		},
		EffectLog: []workflow.EffectRecord{{Owner: "observer"}},
	}

	got := BoardProjection(task)
	if got.Body != task.Body || len(got.AgentRuns) != 1 || got.AgentRuns[0].AgentID != "agent" || got.AgentRuns[0].CostUSD != 1.25 {
		t.Fatalf("card data = %+v", got)
	}
	if got.AgentRuns[0].Prompt != "" || got.AgentRuns[0].Result != "" || got.AgentRuns[0].Provider != "" || got.AgentRuns[0].LogFile != "" {
		t.Fatalf("run retained detail-only data: %+v", got.AgentRuns[0])
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "workflow" || got.Workflow.CurrentStep != "verify_checks" || got.Workflow.State != workflow.ExecRunning {
		t.Fatalf("workflow card state = %+v", got.Workflow)
	}
	if len(got.Workflow.Variables) != 0 || len(got.Workflow.StepHistory) != 0 || len(got.Workflow.EffectLog) != 0 || len(got.Attachments) != 0 || len(got.EffectLog) != 0 {
		t.Fatalf("projection retained detail-only history: %+v", got)
	}
	if task.AgentRuns[0].Prompt == "" || len(task.Workflow.StepHistory) == 0 {
		t.Fatal("BoardProjection mutated its input")
	}
}
