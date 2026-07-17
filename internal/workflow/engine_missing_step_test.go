package workflow

import (
	"strings"
	"testing"
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

	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
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

// TestResumeStalled_MissingStepRetriesPartialEscalation covers the window
// escalateMissingStep's write order opens: the status lands first, so a failed
// SetWorkflow leaves the task human-required with a live execution. The planning
// dispatcher only re-plans over a terminal execution, so that half-applied state
// cannot follow the escalation's own advice — the next tick must finish the job
// rather than skip the task for being human-required.
func TestResumeStalled_MissingStepRetriesPartialEscalation(t *testing.T) {
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

	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	tasks.failSetWorkflow = true
	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required after the first write", ti.Status)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q — the fixture must simulate the failed second write", ti.Workflow.State, ExecWaiting)
	}

	// Once the store recovers, the next tick must complete the escalation.
	tasks.failSetWorkflow = false
	engine.ResumeStalled()

	ti2, _ := tasks.GetTask("t1")
	if ti2.Workflow.State != ExecFailed {
		t.Errorf("State = %q, want %q — a half-applied escalation never retried, so the operator's re-plan stays blocked forever",
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
		status     string
		hasAgent   bool
		wantStatus string
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

			engine := NewEngine(store, tasks, agents, discardLogger())
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
