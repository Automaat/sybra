package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/taskstatus"
)

// missingStepDef registers a workflow whose only step is NOT the one tasks are
// parked on, standing in for a release that deleted a step out from under
// in-flight work.
func missingStepDef(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "gone-wf",
		Name: "gone wf",
		Steps: []Step{
			{ID: "still_here", Name: "Still Here", Type: StepRunAgent, Next: []Transition{{GoTo: ""}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestResumeStalled_MissingStepEscalatesOnce covers the strand a deleted step
// leaves behind: every advance path bails on a nil step, so a task parked there
// is invisible to the operator and to approve/reject alike. The execution must
// end up failed, not merely flagged — the planning dispatcher only starts a
// fresh workflow over a completed or failed one, so a waiting execution would
// reject the re-plan the escalation message tells the operator to perform.
func TestResumeStalled_MissingStepEscalatesOnce(t *testing.T) {
	store := missingStepDef(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "gone-wf",
			CurrentStep: "deleted_by_a_release",
			State:       ExecWaiting,
		},
	})

	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("Status = %q, want human-required — a task on a deleted step must not strand silently", ti.Status)
	}
	if ti.Workflow.State != ExecFailed {
		t.Errorf("State = %q, want %q — the operator's re-plan is rejected over a non-terminal execution", ti.Workflow.State, ExecFailed)
	}
	// The escalation reason is the operator's only instruction; it must name the
	// recovery that actually works.
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "planning") {
		t.Errorf("reason = %q, want it to name the re-plan recovery", reason)
	}

	// A second pass must be inert: the failed execution is skipped outright.
	before := tasks.Reason("t1")
	engine.ResumeStalled()
	ti2, _ := tasks.GetTask("t1")
	if ti2.Workflow.State != ExecFailed || tasks.Reason("t1") != before {
		t.Errorf("second ResumeStalled mutated the task: state=%q reason=%q", ti2.Workflow.State, tasks.Reason("t1"))
	}
}

// TestResumeStalled_MissingStepRetriesAfterPersistFailure covers the window
// escalateMissingStep used to leave open when status and execution were two
// separate store writes: a failed second write could land a `human-required`
// task with a still-waiting execution, which the planning dispatcher refuses
// to re-plan over. SetStatusAndWorkflow persists both fields in one store
// call, so a failed write must now leave the task completely unchanged
// (neither field applied) — and the next tick must retry and fully apply
// both once the store recovers.
func TestResumeStalled_MissingStepRetriesAfterPersistFailure(t *testing.T) {
	store := missingStepDef(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "gone-wf",
			CurrentStep: "deleted_by_a_release",
			State:       ExecWaiting,
		},
	})

	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	tasks.failSetWorkflow = true
	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "planning" {
		t.Fatalf("Status = %q, want unchanged planning — a failed atomic write must not apply either field", ti.Status)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q — a failed atomic write must not apply either field", ti.Workflow.State, ExecWaiting)
	}

	// Once the store recovers, the next tick must complete the escalation.
	tasks.failSetWorkflow = false
	engine.ResumeStalled()

	ti2, _ := tasks.GetTask("t1")
	if ti2.Status != "human-required" {
		t.Errorf("Status = %q, want human-required — the retried escalation must fully apply", ti2.Status)
	}
	if ti2.Workflow.State != ExecFailed {
		t.Errorf("State = %q, want %q — the retried escalation must fully apply",
			ti2.Workflow.State, ExecFailed)
	}
}

// TestResumeStalled_MissingStepSkipsQuietCases pins the escalation's blast
// radius. Nobody waits on a done/cancelled task, and a live agent must keep its
// chance to land a sidecar — HandleAgentComplete bails on a terminal execution,
// so failing one underneath a running agent discards the run.
func TestResumeStalled_MissingStepSkipsQuietCases(t *testing.T) {
	cases := []struct {
		name       string
		status     taskstatus.Status
		hasAgent   bool
		wantStatus taskstatus.Status
	}{
		{name: "cancelled", status: "cancelled", wantStatus: "cancelled"},
		{name: "done", status: "done", wantStatus: "done"},
		{name: "live agent", status: "planning", hasAgent: true, wantStatus: "planning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := missingStepDef(t)
			tasks := newMemTasks()
			tasks.Put(TaskInfo{
				ID:     "t1",
				Status: tc.status,
				Workflow: &Execution{
					WorkflowID:  "gone-wf",
					CurrentStep: "deleted_by_a_release",
					State:       ExecWaiting,
				},
			})
			agents := newMockAgents()
			if tc.hasAgent {
				agents.running["t1"] = "agent-1"
			}

			engine := NewTestEngine(store, tasks, agents, discardLogger())
			engine.ResumeStalled()

			ti, _ := tasks.GetTask("t1")
			if ti.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (escalation must not touch this task)", ti.Status, tc.wantStatus)
			}
			if ti.Workflow.State == ExecFailed {
				t.Error("execution failed — escalation must not fire here")
			}
		})
	}
}

func TestResumeStalled_MissingSnapshotEscalatesOnce(t *testing.T) {
	store := newTestStore(t)
	def, err := store.Get("test-simple")
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
		panic("unreachable")
	}
	hash, err := store.SaveSnapshot(def)
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Definition{
		ID:   "test-simple",
		Name: def.Name,
		Trigger: Trigger{
			On: def.Trigger.On,
		},
		Steps: []Step{
			{ID: "triage", Name: "Changed", Type: StepRunAgent, Config: StepConfig{Role: "triage"}},
		},
	}); err != nil {
		t.Fatalf("Save updated workflow: %v", err)
	}
	snapshotPath, err := store.snapshotPath("test-simple", hash)
	if err != nil {
		t.Fatalf("snapshotPath: %v", err)
		panic("unreachable")
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatalf("Remove snapshot: %v", err)
		panic("unreachable")
	}

	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: taskstatus.Planning,
		Workflow: &Execution{
			WorkflowID:     "test-simple",
			DefinitionHash: hash,
			CurrentStep:    "triage",
			State:          ExecWaiting,
		},
	})

	engine, err := NewEngine(store, tasks, newMockAgents(), discardLogger(), completeDependencies())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
		panic("unreachable")
	}
	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != taskstatus.HumanRequired {
		t.Fatalf("Status = %q, want human-required", ti.Status)
	}
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("State = %q, want %q", ti.Workflow.State, ExecFailed)
	}
	if ti.Blocker.Code != workflowDefinitionSnapshotMissingCode {
		t.Fatalf("blocker code = %q, want %q", ti.Blocker.Code, workflowDefinitionSnapshotMissingCode)
	}

	before := tasks.Reason("t1")
	engine.ResumeStalled()
	ti2, _ := tasks.GetTask("t1")
	if ti2.Workflow.State != ExecFailed || tasks.Reason("t1") != before {
		t.Fatalf("second ResumeStalled mutated task: state=%q reason=%q", ti2.Workflow.State, tasks.Reason("t1"))
	}
}
