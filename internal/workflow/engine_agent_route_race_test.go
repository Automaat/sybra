package workflow

import (
	"errors"
	"sync"
	"testing"
)

func TestExecRunAgent_CompletionRacingRouteRegistrationIsNotDropped(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "todo",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	engine.markStepStarting("t1", "triage")
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow == nil {
		t.Fatal("task workflow is nil after markStepStarting")
	}

	step := &Step{
		ID:     "triage",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "triage", Prompt: "test"},
	}
	wfExec := ti.Workflow.Clone()
	if wfExec == nil {
		t.Fatal("Clone() returned nil workflow")
	}
	ctx := TemplateContext{Task: ti, Step: *step, Vars: wfExec.Variables}

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = engine.execRunAgent("t1", step, wfExec, ctx)
	}()

	<-agents.startEntered
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "triaged"})

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "triage" {
		t.Fatalf("current step = %q, want triage before route registration", got.Workflow.CurrentStep)
	}

	close(agents.startGate)
	wg.Wait()
	if runErr != nil {
		t.Fatalf("execRunAgent: %v", runErr)
	}

	got, err = tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep == "triage" {
		t.Fatal("buffered completion was not replayed after route registration")
	}
}

func TestExecRunAgent_CompletionBufferedAcrossRoutePersistFailureStillAdvances(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "todo",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	engine.markStepStarting("t1", "triage")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	step := &Step{
		ID:     "triage",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "triage", Prompt: "test"},
	}
	wfExec := ti.Workflow.Clone()
	if wfExec == nil {
		t.Fatal("workflow clone is nil")
	}
	ctx := TemplateContext{Task: ti, Step: *step, Vars: wfExec.Variables}

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = engine.execRunAgent("t1", step, wfExec, ctx)
	}()

	<-agents.startEntered
	tasks.failSetWorkflowN = 1
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "triaged"})

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "triage" {
		t.Fatalf("current step = %q, want triage before StartAgent returns", got.Workflow.CurrentStep)
	}

	close(agents.startGate)
	wg.Wait()
	if !errors.Is(runErr, errWorkflowYield) {
		t.Fatalf("execRunAgent err = %v, want errWorkflowYield", runErr)
	}

	got, err = tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep == "triage" {
		t.Fatal("buffered completion was lost after route persistence failed")
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "agent-1"); tracked {
		t.Fatal("agent route still tracked after buffered completion replay")
	}
}
