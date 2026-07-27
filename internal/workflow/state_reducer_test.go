package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReduce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		desired  DesiredState
		observed ObservedState
		check    func(t *testing.T, effects []Effect)
		wantErr  string
	}{
		{
			name: "workflow start dispatches first step",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{ID: "start", Type: StepRunAgent}}},
				WorkflowID: "wf",
				DesiredVars: map[string]string{
					"seed": "value",
				},
			},
			observed: ObservedState{Now: now},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectSetWorkflowState, EffectDispatchStep)
				wf := effects[0].Workflow
				if wf == nil {
					t.Fatal("set-workflow effect missing workflow")
				}
				if wf.WorkflowID != "wf" || wf.CurrentStep != "start" || wf.State != ExecRunning {
					t.Fatalf("workflow = %#v", wf)
				}
				if !wf.StartedAt.Equal(now) {
					t.Fatalf("StartedAt = %s, want %s", wf.StartedAt, now)
				}
				if got := wf.Variables["seed"]; got != "value" {
					t.Fatalf("Variables[seed] = %q, want value", got)
				}
				if effects[1].Step == nil || effects[1].Step.ID != "start" {
					t.Fatalf("dispatch step = %#v, want start", effects[1].Step)
				}
			},
		},
		{
			name: "advance to next step after completion",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{
					{ID: "build", Type: StepRunAgent, Next: []Transition{{GoTo: "review"}}},
					{ID: "review", Type: StepRunAgent},
				}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now: now,
				Execution: &Execution{
					WorkflowID:  "wf",
					CurrentStep: "build",
					State:       ExecRunning,
					Variables:   map[string]string{"keep": "me"},
					StartedAt:   now.Add(-time.Hour),
				},
				CompletedOutput: &StepOutput{StepID: "build", Status: "completed", Output: "ok"},
			},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectRecordStep, EffectSetWorkflowState, EffectDispatchStep)
				if effects[0].Record == nil || effects[0].Record.StepID != "build" || effects[0].Record.Status != "completed" {
					t.Fatalf("record = %#v", effects[0].Record)
				}
				wf := effects[1].Workflow
				if wf == nil || wf.CurrentStep != "review" || wf.State != ExecRunning {
					t.Fatalf("workflow = %#v, want running review", wf)
				}
				if got := wf.Variables["step.build.output"]; got != "ok" {
					t.Fatalf("step output var = %q, want ok", got)
				}
				if effects[2].Step == nil || effects[2].Step.ID != "review" {
					t.Fatalf("dispatch step = %#v, want review", effects[2].Step)
				}
			},
		},
		{
			name: "end transition completes workflow",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{ID: "done", Type: StepRunAgent, Next: []Transition{{GoTo: ""}}}}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:             now,
				Execution:       &Execution{WorkflowID: "wf", CurrentStep: "done", State: ExecRunning, StartedAt: now.Add(-time.Minute)},
				CompletedOutput: &StepOutput{StepID: "done", Status: "completed"},
			},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectRecordStep, EffectCompleteWorkflow)
				wf := effects[1].Workflow
				if wf == nil || wf.State != ExecCompleted || wf.CurrentStep != "" || wf.CompletedAt == nil || !wf.CompletedAt.Equal(now) {
					t.Fatalf("workflow = %#v", wf)
				}
			},
		},
		{
			name: "terminal step output completes workflow immediately",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{ID: "link-pr", Type: StepRunAgent, Next: []Transition{{GoTo: "next"}}}, {ID: "next", Type: StepRunAgent}}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:             now,
				Execution:       &Execution{WorkflowID: "wf", CurrentStep: "link-pr", State: ExecRunning, StartedAt: now.Add(-time.Minute)},
				CompletedOutput: &StepOutput{StepID: "link-pr", Status: "completed", TerminalStatus: "done", TerminalReason: "already merged"},
			},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectRecordStep, EffectSetTaskStatus, EffectCompleteWorkflow)
				if effects[1].Status != "done" || effects[1].StatusReason != "already merged" {
					t.Fatalf("status effect = %#v", effects[1])
				}
				wf := effects[2].Workflow
				if wf == nil || wf.State != ExecCompleted || wf.CurrentStep != "" {
					t.Fatalf("workflow = %#v", wf)
				}
			},
		},
		{
			name: "step set status emits task-status effect",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{
					{ID: "mark-review", Type: StepSetStatus, Config: StepConfig{Status: "in-review", StatusReason: "ready"}, Next: []Transition{{GoTo: "await"}}},
					{ID: "await", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve"}}},
				}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:       now,
				Task:      TaskInfo{ID: "t1", Status: "todo"},
				Execution: &Execution{WorkflowID: "wf", CurrentStep: "mark-review", State: ExecRunning, StartedAt: now.Add(-time.Minute)},
			},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectSetTaskStatus, EffectRecordStep, EffectSetTaskStatus, EffectSetWorkflowState, EffectWaitHuman)
				if effects[0].Status != "in-review" {
					t.Fatalf("first status = %q, want in-review", effects[0].Status)
				}
				if effects[4].Step == nil || effects[4].Step.ID != "await" {
					t.Fatalf("wait effect = %#v", effects[4].Step)
				}
			},
		},
		{
			name: "step wait human emits wait-human effect",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{ID: "review", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}}}}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:       now,
				Task:      TaskInfo{ID: "t1", Status: "todo"},
				Execution: &Execution{WorkflowID: "wf", CurrentStep: "review", State: ExecRunning, StartedAt: now.Add(-time.Minute)},
			},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectSetTaskStatus, EffectSetWorkflowState, EffectWaitHuman)
				if effects[2].Step == nil || effects[2].Step.ID != "review" {
					t.Fatalf("wait step = %#v", effects[2].Step)
				}
				if got := effects[2].HumanActions; !reflect.DeepEqual(got, []string{"approve", "reject"}) {
					t.Fatalf("human actions = %#v", got)
				}
			},
		},
		{
			name: "transition error returns error",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{
					ID:   "gate",
					Type: StepRunAgent,
					Next: []Transition{{When: &Condition{Field: "task.reviewed", Operator: "equals", Value: "true"}, GoTo: "next"}},
				}, {ID: "next", Type: StepRunAgent}}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:             now,
				Task:            TaskInfo{ID: "t1", Reviewed: false},
				Execution:       &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning},
				CompletedOutput: &StepOutput{StepID: "gate", Status: "completed"},
			},
			wantErr: "no matching transition found",
		},
		{
			name: "missing next step returns error",
			desired: DesiredState{
				Definition: Definition{ID: "wf", Steps: []Step{{ID: "gate", Type: StepRunAgent, Next: []Transition{{GoTo: "missing"}}}}},
				WorkflowID: "wf",
			},
			observed: ObservedState{
				Now:             now,
				Execution:       &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning},
				CompletedOutput: &StepOutput{StepID: "gate", Status: "completed"},
			},
			wantErr: "next step missing not found",
		},
		{
			name: "returned effects do not alias inputs",
			desired: DesiredState{
				Definition:  Definition{ID: "wf", Steps: []Step{{ID: "review", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}}}}},
				WorkflowID:  "wf",
				DesiredVars: map[string]string{"seed": "value"},
			},
			observed: ObservedState{Now: now},
			check: func(t *testing.T, effects []Effect) {
				t.Helper()
				requireKinds(t, effects, EffectSetWorkflowState, EffectSetTaskStatus, EffectSetWorkflowState, EffectWaitHuman)
				effects[0].Workflow.Variables["seed"] = "mutated"
				effects[3].HumanActions[0] = "mutated"
				effects[3].Step.Config.HumanActions[0] = "mutated-step"
				if got := effects[0].Workflow.Variables["seed"]; got != "mutated" {
					t.Fatalf("mutation failed, got %q", got)
				}
				if got := effects[3].Step.Config.HumanActions[0]; got != "mutated-step" {
					t.Fatalf("step mutation failed, got %q", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			effects, err := Reduce(tc.desired, tc.observed)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr && !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, effects)
			}
		})
	}

	t.Run("input immutability source objects stay unchanged", func(t *testing.T) {
		t.Parallel()
		desired := DesiredState{
			Definition:  Definition{ID: "wf", Steps: []Step{{ID: "review", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}}}}},
			WorkflowID:  "wf",
			DesiredVars: map[string]string{"seed": "value"},
		}
		observed := ObservedState{Now: now}
		_, err := Reduce(desired, observed)
		if err != nil {
			t.Fatal(err)
		}
		if got := desired.DesiredVars["seed"]; got != "value" {
			t.Fatalf("DesiredVars mutated to %q", got)
		}
		if got := desired.Definition.Steps[0].Config.HumanActions[0]; got != "approve" {
			t.Fatalf("definition mutated to %q", got)
		}
	})
}

func TestReduceTransitionFields(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	makeDesired := func(cond Condition) DesiredState {
		return DesiredState{
			Definition: Definition{ID: "wf", Steps: []Step{
				{ID: "gate", Type: StepCondition, Next: []Transition{{When: &cond, GoTo: "wait"}, {GoTo: ""}}},
				{ID: "wait", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve"}}},
			}},
			WorkflowID:       "wf",
			ReviewUntilClean: true,
		}
	}
	makeObserved := func(task TaskInfo, exec *Execution, exceeded bool) ObservedState {
		return ObservedState{
			Now:                  now,
			Task:                 task,
			Execution:            exec,
			ReviewBudgetExceeded: exceeded,
		}
	}

	cases := []struct {
		name      string
		condition Condition
		observed  ObservedState
	}{
		{
			name:      "task plan critique",
			condition: Condition{Field: "task.plan_critique", Operator: "equals", Value: "approve"},
			observed:  makeObserved(TaskInfo{ID: "t1", PlanCritique: "approve"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, false),
		},
		{
			name:      "task reviewed",
			condition: Condition{Field: "task.reviewed", Operator: "equals", Value: "true"},
			observed:  makeObserved(TaskInfo{ID: "t1", Reviewed: true}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, false),
		},
		{
			name:      "task pr number",
			condition: Condition{Field: "task.pr_number", Operator: "equals", Value: "42"},
			observed:  makeObserved(TaskInfo{ID: "t1", PRNumber: 42}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, false),
		},
		{
			name:      "task code review",
			condition: Condition{Field: "task.code_review", Operator: "equals", Value: "clean"},
			observed:  makeObserved(TaskInfo{ID: "t1", CodeReview: "clean"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, false),
		},
		{
			name:      "review budget exceeded",
			condition: Condition{Field: "task.review_budget_exceeded", Operator: "equals", Value: "true"},
			observed:  makeObserved(TaskInfo{ID: "t1"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, true),
		},
		{
			name:      "workflow vars",
			condition: Condition{Field: "vars.choice", Operator: "equals", Value: "ship"},
			observed:  makeObserved(TaskInfo{ID: "t1"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning, Variables: map[string]string{"choice": "ship"}}, false),
		},
		{
			name:      "vars recovered",
			condition: Condition{Field: "vars.recovered", Operator: "equals", Value: "true"},
			observed:  makeObserved(TaskInfo{ID: "t1"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning, Recovered: true}, false),
		},
		{
			name:      "review until clean config",
			condition: Condition{Field: "config.review_until_clean", Operator: "equals", Value: "true"},
			observed:  makeObserved(TaskInfo{ID: "t1"}, &Execution{WorkflowID: "wf", CurrentStep: "gate", State: ExecRunning}, false),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			effects, err := Reduce(makeDesired(tc.condition), tc.observed)
			if err != nil {
				t.Fatal(err)
			}
			requireKinds(t, effects, EffectRecordStep, EffectSetTaskStatus, EffectSetWorkflowState, EffectWaitHuman)
			if effects[3].Step == nil || effects[3].Step.ID != "wait" {
				t.Fatalf("wait effect = %#v, want wait", effects[3].Step)
			}
		})
	}
}

func requireKinds(t *testing.T, effects []Effect, want ...EffectKind) {
	t.Helper()
	got := make([]EffectKind, len(effects))
	for i := range effects {
		got[i] = effects[i].Kind
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effect kinds = %#v, want %#v", got, want)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
