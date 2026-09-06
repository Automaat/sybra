package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestAdmissionRefusalRenewsPendingEffectAndRetiresOnlyOwnedRoute(t *testing.T) {
	for _, pendingOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "pending-only"}[pendingOnly], func(t *testing.T) {
			tasks := newMemTasks()
			engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
			old := EffectID{Generation: 1, StepID: "triage", Pos: effectPosStepAction}
			wf := &Execution{WorkflowID: "test-simple", CurrentStep: "triage", State: ExecWaiting}
			wf.RecordEffectIntent(old, time.Now())
			if pendingOnly {
				engine.setPendingAgentStep("t1", "refused", "triage")
			} else {
				wf.SetAgentRoute("refused", "triage")
			}
			tasks.Put(TaskInfo{ID: "t1", Generation: 1, Status: taskstatus.InProgress, Workflow: wf})
			completion := AgentCompletion{AgentID: "refused", EscalationReason: "infrastructure_admission_deferred"}
			engine.HandleAgentComplete("t1", completion)
			got, _ := tasks.GetTask("t1")
			rec, ok := got.Workflow.EffectRecordForStep("triage", effectPosStepAction)
			if !ok || rec.ID.Equal(old) || got.Workflow.Variables[workflowRetryAfterVar] == "" {
				t.Fatalf("refusal did not renew pending effect: %+v", got.Workflow)
			}
			if engine.hasPendingAgentRouteForStep("t1", &Step{ID: "triage", Type: StepRunAgent}) {
				t.Fatal("rejected pending route retained")
			}
			before := got.Workflow.Clone()
			engine.HandleAgentComplete("t1", completion)
			got, _ = tasks.GetTask("t1")
			if !reflect.DeepEqual(before, got.Workflow) {
				t.Fatal("duplicate refusal mutated replacement dispatch")
			}
		})
	}
}

func TestParallelAdmissionRefusalResumesWithFreshSlotIdentity(t *testing.T) {
	for _, slotOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable-route", true: "slot-only"}[slotOnly], func(t *testing.T) {
			tasks, agents := newMemTasks(), newMockAgents()
			store := newTestStoreWith(t, "test-parallel.yaml")
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			fc := clock.NewFake(time.Now().UTC())
			engine.SetClock(fc)
			wf := &Execution{WorkflowID: "test-parallel", CurrentStep: "plan", State: ExecWaiting,
				ParallelInflight: map[string]*ParallelChildren{"plan": {ParentStepID: "plan", Children: map[string]*ChildStatus{
					"plan_a": {Status: "pending", AgentID: "refused", Provider: "claude"},
					"plan_b": {Status: "completed", AgentID: "done", Provider: "codex"},
				}}}}
			old := EffectID{Generation: 1, StepID: "plan", Pos: effectPosStepAction}
			wf.RecordEffectIntent(old, fc.Now())
			wf.RecordEffectCompletion(old, fc.Now())
			if !slotOnly {
				wf.SetAgentRoute("refused", "plan_a")
			}
			tasks.Put(TaskInfo{ID: "t1", Generation: 1, Status: taskstatus.InProgress, AgentMode: "headless", Workflow: wf})
			completion := AgentCompletion{AgentID: "refused", EscalationReason: "infrastructure_admission_deferred"}
			engine.HandleAgentComplete("t1", completion)
			engine.HandleAgentComplete("t1", completion)
			got, _ := tasks.GetTask("t1")
			children := got.Workflow.ParallelInflight["plan"].Children
			if children["plan_a"].AdmissionGeneration != 1 || children["plan_a"].Retries != 0 || children["plan_b"].AdmissionGeneration != 0 {
				t.Fatalf("incorrect slot retirement: %+v", children)
			}
			// Reconstruct the engine to prove identity survives a leader restart.
			engine = NewTestEngine(store, tasks, agents, discardLogger())
			engine.SetClock(fc)
			fc.Advance(time.Minute)
			engine.ResumeStalled()
			if agents.CallCount() != 1 || agents.LastCall().Assignment.IntentID != "t1:test-parallel:parallel:plan:plan_a:admission:1" {
				t.Fatalf("retry did not start only the refused slot: %+v", agents.LastCall())
			}
		})
	}
}
