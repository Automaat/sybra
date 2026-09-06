package workflow

import (
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestAdmissionBestOfNDeferralWithLiveSibling(t *testing.T) {
	for _, ownership := range []string{"route", "slot", "pending"} {
		t.Run(ownership, func(t *testing.T) {
			engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
			fc := clock.NewFake(time.Now().UTC())
			engine.SetClock(fc)
			wf := &Execution{WorkflowID: "bestofn-test", CurrentStep: "attempts", State: ExecWaiting, BestOfNInflight: map[string]*BestOfNInflight{
				"attempts": {ParentStepID: "attempts", Attempts: map[string]*AttemptStatus{
					"attempt_1": {AttemptID: "attempt_1", Status: "pending", AgentID: "refused", Provider: providerid.Claude},
					"attempt_2": {AttemptID: "attempt_2", Status: "pending", AgentID: "sibling", Provider: providerid.Codex},
				}},
			}}
			old := EffectID{Generation: 1, StepID: "attempts", Pos: effectPosStepAction}
			wf.RecordEffectIntent(old, fc.Now())
			wf.RecordEffectCompletion(old, fc.Now())
			key := bestOfNAttemptStepKey("attempts", "attempt_1")
			switch ownership {
			case "route":
				wf.SetAgentRoute("refused", key)
			case "pending":
				wf.BestOfNInflight["attempts"].Attempts["attempt_1"].AgentID = ""
				engine.setPendingAgentStep("t1", "refused", key)
			}
			wf.SetAgentRoute("sibling", bestOfNAttemptStepKey("attempts", "attempt_2"))
			tasks.Put(TaskInfo{ID: "t1", Generation: 1, Status: taskstatus.InProgress, AgentMode: "headless", Workflow: wf})
			agents.running["t1"] = "sibling"
			refusal := AgentCompletion{AgentID: "refused", EscalationReason: "infrastructure_admission_deferred"}
			engine.HandleAgentComplete("t1", refusal)
			engine.HandleAgentComplete("t1", refusal)
			fc.Advance(time.Minute)
			engine.ResumeStalled()
			if agents.CallCount() != 0 || agents.running["t1"] != "sibling" {
				t.Fatal("deferral disturbed live sibling")
			}
			got, _ := tasks.GetTask("t1")
			slots := got.Workflow.BestOfNInflight["attempts"].Attempts
			if slots["attempt_1"].AdmissionGeneration != 1 || slots["attempt_1"].Retries != 0 || slots["attempt_2"].AgentID != "sibling" {
				t.Fatalf("wrong deferred slots: %+v", slots)
			}
			delete(agents.running, "t1")
			engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "sibling", Success: true, Result: "implemented"})
			engine.ResumeStalled()
			if agents.CallCount() != 1 || agents.LastCall().Assignment.IntentID != "t1:bestofn-test:bestofn:attempts:attempt_1:admission:1" {
				t.Fatalf("bestofN retry=%+v calls=%d", agents.LastCall(), agents.CallCount())
			}
		})
	}
}

type admissionFastRefusalAgents struct{ *mockAgents }

func (a *admissionFastRefusalAgents) StartAgent(taskID, role, mode, model, provider, prompt, dir string, tools []string, needsWorktree, oneShot bool, schema, cleanRef string, assignment AgentAssignment) (agentID, startedDir, baselineRef string, runErr error) {
	id, runDir, base, err := a.mockAgents.StartAgent(taskID, role, mode, model, provider, prompt, dir, tools, needsWorktree, oneShot, schema, cleanRef, assignment)
	a.StopAgentsForTask(taskID, "")
	return id, runDir, base, err
}
func TestAdmissionFastRefusalBeforeEffectCompletion(t *testing.T) {
	tasks := newMemTasks()
	agents := &admissionFastRefusalAgents{newMockAgents()}
	engine := NewTestEngine(newTestStore(t), tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	before, _ := tasks.GetTask("t1")
	old, ok := before.Workflow.EffectRecordForStep("triage", effectPosStepAction)
	if !ok || old.CompletedAt != nil {
		t.Fatalf("fixture did not leave pending dispatch %+v", old)
	}
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", EscalationReason: "infrastructure_admission_deferred"})
	after, _ := tasks.GetTask("t1")
	fresh, ok := after.Workflow.EffectRecordForStep("triage", effectPosStepAction)
	if !ok || fresh.ID.Equal(old.ID) || len(after.Workflow.AgentRoutes) != 0 || after.Workflow.CountStep("triage") != 0 {
		t.Fatalf("fast refusal not safely renewed %+v", after.Workflow)
	}
}

type admissionPauseSecondAdmission struct {
	*mockAgents
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (a *admissionPauseSecondAdmission) AdmitDispatch(taskID, role, mode string) (admitted bool, reason string) {
	a.calls++
	if a.calls == 2 {
		close(a.entered)
		<-a.release
	}
	return a.mockAgents.AdmitDispatch(taskID, role, mode)
}
func TestAdmissionParallelPublicationPreservesFastRefusal(t *testing.T) {
	tasks := newMemTasks()
	agents := &admissionPauseSecondAdmission{mockAgents: newMockAgents(), entered: make(chan struct{}), release: make(chan struct{})}
	engine := NewTestEngine(newTestStoreWith(t, "test-parallel.yaml"), tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})
	var once sync.Once
	release := func() { once.Do(func() { close(agents.release) }) }
	defer release()
	started := make(chan error, 1)
	go func() { started <- engine.StartWorkflow("t1", "test-parallel") }()
	select {
	case <-agents.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second child preparation not reached")
	}
	completion := make(chan struct{})
	go func() {
		engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", EscalationReason: "infrastructure_admission_deferred"})
		close(completion)
	}()
	// The refusal must wait for original fan-out publication. Keep its
	// completion asynchronous so this test does not deadlock correct locking.
	completedEarly := false
	select {
	case <-completion:
		completedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fan-out did not finish")
	}
	select {
	case <-completion:
	case <-time.After(2 * time.Second):
		t.Fatal("deferral did not finish")
	}
	if completedEarly {
		t.Fatal("refusal completed before original fan-out publication released")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.ParallelInflight["plan"] == nil {
		t.Fatal("parallel publication disappeared")
	}
	parent := got.Workflow.ParallelInflight["plan"]
	child := parent.Children["plan_a"]
	if child == nil || child.AdmissionGeneration != 1 || child.AgentID != "" {
		t.Fatalf("parallel publication overwrote refusal: %+v", child)
	}
	if sibling := parent.Children["plan_b"]; sibling == nil || sibling.AgentID == "" {
		t.Fatal("second sibling disappeared")
	}
}
