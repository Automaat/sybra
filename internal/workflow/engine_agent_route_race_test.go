package workflow

import (
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

	step := &Step{
		ID:     "triage",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "triage", Prompt: "test"},
	}
	wfExec := &Execution{WorkflowID: "test-simple", CurrentStep: "triage", State: ExecWaiting, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1", Status: "todo"}, Step: *step, Vars: wfExec.Variables}

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
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.CurrentStep != "triage" {
		t.Fatalf("workflow advanced before route registration: current_step = %q", got.Workflow.CurrentStep)
	}

	close(agents.startGate)
	wg.Wait()
	if runErr != nil {
		t.Fatalf("execRunAgent: %v", runErr)
	}

	got, err = tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.CurrentStep == "triage" {
		t.Fatal("buffered completion was never replayed")
	}
}

func TestResolveCompletionRoute_BufferedBeforeUnmarkIsDelivered(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.markStepStarting("t1", "s1")
	c := AgentCompletion{AgentID: "agent-1", Success: true}
	if _, status := engine.resolveCompletionRoute("t1", "s1", c); status != taskStepBuffered {
		t.Fatalf("status = %v, want taskStepBuffered", status)
	}

	popped := engine.unmarkStepStartingAndTakePending("t1", "s1")
	if len(popped) != 1 || popped[0].AgentID != "agent-1" {
		t.Fatalf("popped = %+v, want exactly the buffered completion", popped)
	}
}

func TestResolveCompletionRoute_NeverStrandsAcrossUnmarkRace(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.markStepStarting("t1", "s1")

	popped := engine.unmarkStepStartingAndTakePending("t1", "s1")
	if len(popped) != 0 {
		t.Fatalf("popped = %+v, want none", popped)
	}

	c := AgentCompletion{AgentID: "agent-1", Success: true}
	if _, status := engine.resolveCompletionRoute("t1", "s1", c); status == taskStepBuffered {
		t.Fatal("completion was buffered after its start's unmark already ran")
	}
}

func TestResolveCompletionRoute_OwnRouteOverridesRoutedPhantomCheck(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.mu.Lock()
	engine.agentRoutes["agent-1"] = agentRoute{taskID: "t1", stepID: "s1"}
	engine.mu.Unlock()

	c := AgentCompletion{AgentID: "agent-1", Success: true}
	spawnedStep, status := engine.resolveCompletionRoute("t1", "s1", c)
	if status != taskStepTracked {
		t.Fatalf("status = %v, want taskStepTracked", status)
	}
	if spawnedStep != "s1" {
		t.Fatalf("spawnedStep = %q, want s1", spawnedStep)
	}
}

func TestClearAgentStepsForTask_DropsBufferedCompletions(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.markStepStarting("t1", "s1")
	c := AgentCompletion{AgentID: "agent-1", Success: true}
	if _, status := engine.resolveCompletionRoute("t1", "s1", c); status != taskStepBuffered {
		t.Fatalf("status = %v, want taskStepBuffered", status)
	}

	engine.clearAgentStepsForTask("t1")

	engine.markStepStarting("t1", "s1")
	popped := engine.unmarkStepStartingAndTakePending("t1", "s1")
	if len(popped) != 0 {
		t.Fatalf("popped = %+v, want none", popped)
	}
}
