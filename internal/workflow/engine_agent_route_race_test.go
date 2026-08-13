package workflow

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestHandleAgentComplete_WaitsForRunAgentRoutePublication(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})

	startDone := make(chan error, 1)
	go func() {
		startDone <- engine.StartWorkflow("t1", "test-simple")
	}()

	<-agents.startEntered
	completeDone := make(chan struct{})
	go func() {
		engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "triaged"})
		close(completeDone)
	}()

	select {
	case <-completeDone:
		t.Fatal("completion advanced before route publication finished")
	case <-time.After(50 * time.Millisecond):
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow == nil || ti.Workflow.CurrentStep != "triage" {
		t.Fatalf("current step = %v, want triage while StartAgent is still blocked", ti.Workflow)
	}

	close(agents.startGate)
	if err := <-startDone; err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	<-completeDone

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep == "triage" {
		t.Fatalf("workflow did not advance after completion: %+v", got.Workflow)
	}
}

// A workflow start holds a separate per-task marker while its first step is
// still being dispatched. Recovery must treat that marker as an active
// execution: the effect intent has already been persisted, but there is not
// yet a registered agent or route for the other recovery guards to observe.
// Replaying in this window used to execute the same step twice and let the two
// completions advance the workflow along different branches.
func TestReplayPersistedEffects_DoesNotRaceInitialWorkflowStart(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	startDone := make(chan error, 1)
	go func() {
		startDone <- engine.StartWorkflow("t1", "test-simple")
	}()
	<-agents.startEntered

	replayDone := make(chan bool, 1)
	go func() {
		replayDone <- engine.ReplayPersistedEffectsForTask("t1")
	}()
	select {
	case handled := <-replayDone:
		if !handled {
			t.Fatal("ReplayPersistedEffectsForTask returned false, want starting workflow to consume recovery tick")
		}
	case <-time.After(time.Second):
		t.Fatal("effect replay entered the blocked StartAgent call instead of yielding to the workflow start")
	}
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls while initial start is blocked = %d, want 0", got)
	}

	close(agents.startGate)
	if err := <-startDone; err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent calls after start completes = %d, want exactly 1", got)
	}
}

func TestHandleAgentComplete_AfterRoutePersistFailureStillAdvances(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	startDone := make(chan error, 1)
	go func() {
		startDone <- engine.StartWorkflow("t1", "test-simple")
	}()

	<-agents.startEntered
	tasks.failSetWorkflowN = 1
	completeDone := make(chan struct{})
	go func() {
		engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "triaged"})
		close(completeDone)
	}()

	close(agents.startGate)
	if err := <-startDone; err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	<-completeDone

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep == "triage" {
		t.Fatalf("workflow did not advance after deferred route publication: %+v", got.Workflow)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "agent-1"); tracked {
		t.Fatal("agent route still tracked after completion")
	}
	if _, tracked := engine.pendingRoutes[pendingAgentRouteKey("t1", "agent-1")]; tracked {
		t.Fatal("pending route still tracked after completion")
	}
}

func TestPersistStartedAgent_ClearsStalePendingRouteOnSuccessfulPersist(t *testing.T) {
	store := newTestStore(t)
	def, err := store.Get("test-simple")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("implement")
	if step == nil {
		t.Fatal("implement step missing")
	}

	tasks := newMemTasks()
	wfExec := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless", Workflow: wfExec})

	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.setPendingAgentStep("t1", "agent-1", "implement")

	if err := engine.persistStartedAgent("t1", step, wfExec, "agent-1", "claude", "", "", "", "", ""); err != nil {
		t.Fatalf("persistStartedAgent: %v", err)
	}
	if stepID, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "agent-1"); !tracked || stepID != "implement" {
		t.Fatalf("workflow route = (%q,%v), want implement,true", stepID, tracked)
	}
	if _, tracked := engine.pendingRoutes[pendingAgentRouteKey("t1", "agent-1")]; tracked {
		t.Fatal("pending route still tracked after successful persist")
	}
}
