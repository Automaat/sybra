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

// TestResolveCompletionRoute_BufferedBeforeUnmarkIsDelivered is the happy
// path underlying the test above, isolated at the bookkeeping level: a
// completion that arrives while a start is pending is captured, and the
// matching unmark call pops exactly it back out.
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

// TestResolveCompletionRoute_NeverStrandsAcrossUnmarkRace is the first
// adversarial review's finding: a prior version checked pendingStepStart and
// appended to pendingCompletions as two separate locked operations, so an
// unmark that won the race against the buffer write would pop an empty list,
// and the completion would then land in a pendingCompletions entry nothing
// would ever pop again — a silent, permanent stall reproducing #2176 through
// the fix meant to close it.
//
// resolveCompletionRoute and unmarkStepStartingAndTakePending now share one
// lock acquisition each, so the two operations can't interleave — this test
// drives them in the exact bad order (unmark first, buffer attempt second)
// and asserts the completion is reported taskStepFree, not silently
// swallowed into taskStepBuffered.
func TestResolveCompletionRoute_NeverStrandsAcrossUnmarkRace(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.markStepStarting("t1", "s1")

	popped := engine.unmarkStepStartingAndTakePending("t1", "s1")
	if len(popped) != 0 {
		t.Fatalf("popped = %+v, want none: nothing was buffered yet", popped)
	}

	c := AgentCompletion{AgentID: "agent-1", Success: true}
	if _, status := engine.resolveCompletionRoute("t1", "s1", c); status == taskStepBuffered {
		t.Fatal("completion was buffered after its start's unmark already ran — nothing will ever pop it, reproducing #2176")
	}
}

// TestResolveCompletionRoute_OwnRouteOverridesRoutedPhantomCheck is the
// second adversarial review's finding: HandleAgentComplete used to look up
// c.AgentID's own route (lookupAgentStep) and then, if that missed, run the
// untracked-fallback check (then checkOrBufferForTaskStep) as a *separate*
// critical section. A route for c.AgentID registering in the gap between the
// two made the second call see its own agent's brand-new route as "some
// other agent already owns this step" and delete it via clearAgentStep —
// dropping the completion permanently. resolveCompletionRoute checks
// agentRoutes[c.AgentID] directly, inside the same critical section as the
// task+step scan, so an agent with its own registered route is always
// reported taskStepTracked, never taskStepRouted.
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
		t.Fatalf("status = %v, want taskStepTracked: the completion's own agent owns this exact route", status)
	}
	if spawnedStep != "s1" {
		t.Fatalf("spawnedStep = %q, want s1", spawnedStep)
	}
}

// TestClearAgentStepsForTask_DropsBufferedCompletions covers the first
// adversarial review's third finding: a completion buffered for a superseded
// dispatch attempt must not survive the (re)dispatch that stops and
// supersedes it, the same way its agentRoutes entry does not.
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

	// A fresh dispatch marks its own start next; its unmark must not
	// resurrect the completion the clear above already dropped.
	engine.markStepStarting("t1", "s1")
	popped := engine.unmarkStepStartingAndTakePending("t1", "s1")
	if len(popped) != 0 {
		t.Fatalf("popped = %+v, want none: clearAgentStepsForTask should have dropped the superseded completion", popped)
	}
}
