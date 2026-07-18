package workflow

import (
	"strings"
	"testing"
)

func newPlanCritiqueReconcileDef() Definition {
	return Definition{
		ID:   "plan-critique-reconcile",
		Name: "plan critique reconcile",
		Steps: []Step{
			{
				ID:   "critique_plan",
				Name: "Critique Plan",
				Type: StepRunAgent,
				Next: []Transition{{GoTo: "require_plan_critique"}},
			},
			{
				ID:   "require_plan_critique",
				Name: "Require Plan Critique",
				Type: StepRequireSidecar,
				Config: StepConfig{
					Sidecar:      "plan_critique",
					AllowMissing: true,
				},
				Next: []Transition{
					{
						When: &Condition{
							Field:    "vars.step.require_plan_critique.output",
							Operator: "contains",
							Value:    "skipped",
						},
						GoTo: "review_plan",
					},
					{
						When: &Condition{
							Field:    "task.status",
							Operator: "equals",
							Value:    "human-required",
						},
						GoTo: "",
					},
					{GoTo: "flag_plan_critique_verdict"},
				},
			},
			{
				ID:   "flag_plan_critique_verdict",
				Name: "Flag Plan Critique Verdict",
				Type: StepFlagPlanCritique,
				Next: []Transition{{GoTo: "review_plan"}},
			},
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}
}

func newMaybeCritiqueResumeDef() Definition {
	return Definition{
		ID:   "maybe-critique-resume",
		Name: "maybe critique resume",
		Steps: []Step{
			{
				ID:   "maybe_critique",
				Name: "Maybe Critique",
				Type: StepCondition,
				Next: []Transition{
					{
						When: &Condition{
							Field:    "task.tags",
							Operator: "not_contains",
							Value:    "nocritic",
						},
						GoTo: "critique_plan",
					},
					{GoTo: "review_plan"},
				},
			},
			{
				ID:   "critique_plan",
				Name: "Critique Plan",
				Type: StepRunAgent,
				Next: []Transition{{GoTo: "review_plan"}},
			},
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}
}

func TestHandleStatusChange_ReconcilesCurrentStepToVisibleWaitHuman(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
	if got.Status != "plan-review" {
		t.Fatalf("Status = %q, want plan-review", got.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}

func TestHandleStatusChange_ReconcileExecutesPlanCritiqueFlag(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "plan-review",
		Body:         "original body",
		PlanCritique: "# Plan Review: REFINE\n\nMissing verification.",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if !strings.Contains(got.Body, "Plan Critic Verdict: REFINE") {
		t.Fatalf("Body = %q, want critique warning", got.Body)
	}
	flagOutput := got.Workflow.Variables["step.flag_plan_critique_verdict.output"]
	if !strings.Contains(flagOutput, "verdict: REFINE") || !strings.Contains(flagOutput, "flagged") {
		t.Fatalf("flag output = %q, want flagged verdict", got.Workflow.Variables["step.flag_plan_critique_verdict.output"])
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}

func TestHandleStatusChange_DoesNotReconcileRunningRunAgentWithoutWaitForStatus(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "plan-review",
		Body:         "original body",
		PlanCritique: "# Plan Review: REFINE\n\nMissing verification.",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecRunning,
			Variables:   map[string]string{},
		},
	})
	agents.running["t1"] = "agent-1"

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "critique_plan" {
		t.Fatalf("CurrentStep = %q, want critique_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecRunning {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecRunning)
	}
	if got.Body != "original body" {
		t.Fatalf("Body = %q, want original body", got.Body)
	}
	if _, ok := got.Workflow.Variables["step.flag_plan_critique_verdict.output"]; ok {
		t.Fatal("flag_plan_critique_verdict output recorded while critique agent was still running")
	}
}

func TestHandleStatusChange_ReconcileExecutesCurrentPlanCritiqueFlag(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "plan-review",
		Body:         "original body",
		PlanCritique: "## Verdict: REJECT\n\nMissing safety analysis.",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "flag_plan_critique_verdict",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if !strings.Contains(got.Body, "Plan Critic Verdict: REJECT") {
		t.Fatalf("Body = %q, want critique warning", got.Body)
	}
	flagOutput := got.Workflow.Variables["step.flag_plan_critique_verdict.output"]
	if !strings.Contains(flagOutput, "verdict: REJECT") || !strings.Contains(flagOutput, "flagged") {
		t.Fatalf("flag output = %q, want flagged verdict", flagOutput)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}

func TestResumeStalled_DoesNotReconcileRunningRunAgentWithoutWaitForStatus(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "plan-review",
		Body:         "original body",
		PlanCritique: "# Plan Review: REJECT\n\nMissing safety analysis.",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecRunning,
			Variables:   map[string]string{},
		},
	})
	agents.running["t1"] = "agent-1"

	engine.ResumeStalled()

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "critique_plan" {
		t.Fatalf("CurrentStep = %q, want critique_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecRunning {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecRunning)
	}
	if got.Body != "original body" {
		t.Fatalf("Body = %q, want original body", got.Body)
	}
	if _, ok := got.Workflow.Variables["step.flag_plan_critique_verdict.output"]; ok {
		t.Fatal("flag_plan_critique_verdict output recorded while critique agent was still running")
	}
}

func TestResumeStalled_ReconcilesCurrentStepToVisibleWaitHuman(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.ResumeStalled()

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}

func TestHandleHumanAction_ReconcilesCurrentStepToVisibleWaitHuman(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatalf("HandleHumanAction: %v", err)
	}

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("CurrentStep = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecCompleted)
	}
	if got.Status != "plan-review" {
		t.Fatalf("Status = %q, want plan-review", got.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}

func TestResumeStalled_RechecksPriorConditionBeforeReDispatch(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newMaybeCritiqueResumeDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Tags:   []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID:  "maybe-critique-resume",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StepHistory: []StepRecord{{StepID: "maybe_critique", Status: "completed"}},
		},
	})

	engine.ResumeStalled()

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
	if got.Status != "plan-review" {
		t.Fatalf("Status = %q, want plan-review", got.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("StartAgent called %d times, want 0", agents.CallCount())
	}
}
