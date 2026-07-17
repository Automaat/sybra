package workflow

import (
	"sync"
	"testing"
)

// TestExecRunAgent_CompletionRacingRouteRegistrationIsNotDropped reproduces
// #2176. execRunAgent marks a step "starting" before calling StartAgent, but
// only registers agentRoutes[agentID] after StartAgent returns. A completion
// for that very agent can arrive in between — a near-instant process, or (as
// here) a test harness that controls the timing directly — and used to hit
// HandleAgentComplete's untracked-phantom guard, which saw the pending start
// and dropped the completion outright. The workflow then waited forever for
// a completion that had already happened.
//
// The fix buffers that completion instead of dropping it, and execRunAgent's
// deferred cleanup replays it once the route is registered. This test drives
// the real race with a gated mock StartAgent rather than asserting against
// internal state directly, so it fails the same way #2176 did if the replay
// regresses.
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

	// execRunAgent has called markStepStarting and is now blocked inside
	// StartAgent, exactly the window agentRoutes isn't registered in yet.
	<-agents.startEntered

	// mockAgents assigns IDs deterministically ("agent-N", one call in so
	// far), so the completion this agent will eventually report can be
	// predicted and delivered before StartAgent even returns.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "triaged"})

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.CurrentStep != "triage" {
		t.Fatalf("workflow advanced from a completion that arrived before its own agent was registered: current_step = %q", got.Workflow.CurrentStep)
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
		t.Fatal("buffered completion was never replayed: workflow is stuck on triage forever, reproducing #2176")
	}
}
