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

func newSideEffectReconcileDef() Definition {
	return Definition{
		ID:   "side-effect-reconcile",
		Name: "side effect reconcile",
		Steps: []Step{
			{
				ID:   "route",
				Name: "Route",
				Type: StepCondition,
				Next: []Transition{{GoTo: "create_pr"}},
			},
			{
				ID:   "create_pr",
				Name: "Create PR",
				Type: StepCreatePR,
				Next: []Transition{{GoTo: "review_pr"}},
			},
			{
				ID:     "review_pr",
				Name:   "Review PR",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "ready-pr", HumanActions: []string{"approve"}},
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

func TestHandleStatusChange_ReconcileSideEffectFencedByActiveClaim(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newPlanCritiqueReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engineA := NewEngine(store, tasks, agents, discardLogger())
	engineB := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "plan-review",
		Body:         "original body",
		PlanCritique: "# Plan Review: REJECT\n\nMissing verification.",
		Workflow: &Execution{
			WorkflowID:  "plan-critique-reconcile",
			CurrentStep: "flag_plan_critique_verdict",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	taskInfo, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if _, _, err := engineB.claimStepEffect("t1", taskInfo, "flag_plan_critique_verdict", effectPosStepAction); err != nil {
		t.Fatalf("foreign claim: %v", err)
	}

	engineA.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "flag_plan_critique_verdict" {
		t.Fatalf("CurrentStep = %q, want flag_plan_critique_verdict", got.Workflow.CurrentStep)
	}
	if got.Body != "original body" {
		t.Fatalf("Body = %q, want original body", got.Body)
	}
	if _, ok := got.Workflow.Variables["step.flag_plan_critique_verdict.output"]; ok {
		t.Fatal("flag output recorded despite active foreign claim")
	}
	if len(got.Workflow.EffectLog) != 1 || got.Workflow.EffectLog[0].Owner != engineB.ownerID {
		t.Fatalf("effect log = %+v, want single active claim owned by engineB", got.Workflow.EffectLog)
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

func newParallelReconcileDef() Definition {
	return Definition{
		ID:   "parallel-reconcile",
		Name: "parallel reconcile",
		Steps: []Step{
			{
				ID:   "fan_out",
				Name: "Fan Out",
				Type: StepParallel,
				Parallel: []Step{
					{ID: "child_a", Type: StepRunAgent},
					{ID: "child_b", Type: StepRunAgent},
				},
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

func newBestOfNReconcileDef() Definition {
	return Definition{
		ID:   "bestofn-reconcile",
		Name: "best of n reconcile",
		Steps: []Step{
			{
				ID:   "attempts",
				Name: "Attempts",
				Type: StepBestOfN,
				Config: StepConfig{
					Attempts: 2,
					Prompt:   "implement",
				},
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

// TestHandleStatusChange_DoesNotReconcilePastIncompleteParallel covers a real
// incident on a fix for reconcileCurrentStepFromStatus itself: an external
// task.Status write matching a downstream wait_human must not fast-forward
// CurrentStep past a StepParallel/StepBestOfN boundary while children/
// attempts are still in flight — findReachableWaitHumanByStatus's own walk
// only stops at a *later* async-boundary step it encounters, never at the
// starting step, so without an explicit guard this exact status match would
// jump straight to review_plan while child_b is still pending.
func TestHandleStatusChange_DoesNotReconcilePastIncompleteParallel(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newParallelReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "parallel-reconcile",
			CurrentStep: "fan_out",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			ParallelInflight: map[string]*ParallelChildren{
				"fan_out": {
					ParentStepID: "fan_out",
					Children: map[string]*ChildStatus{
						"child_a": {Status: "completed"},
						"child_b": {Status: "pending"},
					},
				},
			},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "fan_out" {
		t.Fatalf("CurrentStep = %q, want fan_out (must not skip past incomplete parallel children)", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
}

// TestHandleStatusChange_ReconcilesPastCompletedParallel is the control case:
// once every child has reached a terminal status, the same status match must
// still reconcile normally (the guard must not block forever).
func TestHandleStatusChange_ReconcilesPastCompletedParallel(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newParallelReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "parallel-reconcile",
			CurrentStep: "fan_out",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			ParallelInflight: map[string]*ParallelChildren{
				"fan_out": {
					ParentStepID: "fan_out",
					Children: map[string]*ChildStatus{
						"child_a": {Status: "completed"},
						"child_b": {Status: "completed"},
					},
				},
			},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
}

// TestHandleStatusChange_DoesNotReconcilePastNotYetSpawnedParallel covers the
// pre-spawn race an adversarial review pass on this fix surfaced:
// resolveNext can persist CurrentStep pointing at a StepParallel step before
// execParallel has created its ParallelInflight record (a separate,
// unsynchronized write). A missing record must therefore report incomplete
// (block), not complete — treating it as complete here would let
// reconciliation skip the boundary during exactly that window, and the
// children's eventual completions would then have no record to land in.
func TestHandleStatusChange_DoesNotReconcilePastNotYetSpawnedParallel(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newParallelReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "parallel-reconcile",
			CurrentStep: "fan_out",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			// No ParallelInflight entry at all — the pre-spawn window.
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "fan_out" {
		t.Fatalf("CurrentStep = %q, want fan_out (must not skip a boundary whose record hasn't been created yet)", got.Workflow.CurrentStep)
	}
}

func TestHandleStatusChange_DoesNotReconcilePastNotYetSpawnedBestOfN(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newBestOfNReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "bestofn-reconcile",
			CurrentStep: "attempts",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			// No BestOfNInflight entry at all — the pre-spawn window.
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "attempts" {
		t.Fatalf("CurrentStep = %q, want attempts (must not skip a boundary whose record hasn't been created yet)", got.Workflow.CurrentStep)
	}
}

func TestHandleStatusChange_ReconcilesPastCompletedBestOfN(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newBestOfNReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "bestofn-reconcile",
			CurrentStep: "attempts",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			BestOfNInflight: map[string]*BestOfNInflight{
				"attempts": {
					ParentStepID: "attempts",
					Attempts: map[string]*AttemptStatus{
						"attempt_1": {Status: "completed"},
						"attempt_2": {Status: "failed"},
					},
				},
			},
		},
	})

	engine.HandleStatusChange("t1", "plan-review")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
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

func TestHandleStatusChange_DoesNotSkipSideEffectStepToWaitHuman(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newSideEffectReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "ready-pr",
		Workflow: &Execution{
			WorkflowID:  "side-effect-reconcile",
			CurrentStep: "route",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.HandleStatusChange("t1", "ready-pr")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "route" {
		t.Fatalf("CurrentStep = %q, want route", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
}

func TestHandleStatusChange_DoesNotSkipCurrentSideEffectStepToWaitHuman(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(newSideEffectReconcileDef()); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "ready-pr",
		Workflow: &Execution{
			WorkflowID:  "side-effect-reconcile",
			CurrentStep: "create_pr",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	engine.HandleStatusChange("t1", "ready-pr")

	got, _ := tasks.GetTask("t1")
	if got.Workflow.CurrentStep != "create_pr" {
		t.Fatalf("CurrentStep = %q, want create_pr", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
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
