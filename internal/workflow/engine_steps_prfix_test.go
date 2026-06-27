package workflow

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyPRFixResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		output     string
		wantHuman  bool
		wantReason string
	}{
		{
			name: "sentinel human required with reason",
			output: "Aborted rebase.\nSYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: 5 conflicting files exceed the auto-resolve limit\n",
			wantHuman:  true,
			wantReason: "5 conflicting files exceed the auto-resolve limit",
		},
		{
			name:      "sentinel continue",
			output:    "Pushed fixes.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantHuman: false,
		},
		{
			name: "legacy conflict abort text",
			output: "The rebase produced 5 conflicting files, which exceeds the limit of 3. " +
				"As instructed, I ran git rebase --abort. This task requires human review.",
			wantHuman:  true,
			wantReason: "pr-fix agent requested human review: The rebase produced 5 conflicting files, which exceeds the limit of 3. As instructed, I ran git rebase --abort. This task requires human review.",
		},
		{
			name:      "negative human phrase",
			output:    "The conflict is resolved; no human review is required.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantHuman: false,
		},
		{
			name: "last sentinel wins",
			output: "Example contract:\nSYBRA_PR_FIX_RESULT: human-required\n\nActual result:\n" +
				"SYBRA_PR_FIX_RESULT: continue\n",
			wantHuman: false,
		},
		{
			name: "last reason wins",
			output: "SYBRA_PR_FIX_REASON: example only\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: real blocker\n",
			wantHuman:  true,
			wantReason: "real blocker",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotHuman, gotReason := classifyPRFixResult(tc.output)
			if gotHuman != tc.wantHuman {
				t.Fatalf("human = %v, want %v", gotHuman, tc.wantHuman)
			}
			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestExecRoutePRFixResult_HumanRequiredStopsBeforeRelink(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Aborted - the rebase hit 5 conflicting files. Human review is required.",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		PRNumber: 1178,
		Workflow: wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf)
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want human-required reason")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "Human review is required") {
		t.Errorf("reason = %q, want agent output excerpt", reason)
	}
}

func TestPRFixRequiresHuman_UsesLastAgentStepVars(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{
			{
				StepID:  "old_fix",
				Status:  "completed",
				Output:  "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: stale\n",
				AgentID: "agent-1",
			},
			{
				StepID:  "repair_conflicts",
				Status:  "completed",
				Output:  "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: ignored because explicit false var wins\n",
				AgentID: "agent-2",
			},
		},
		Variables: map[string]string{
			"step.old_fix.pr_fix_requires_human":          "true",
			"step.old_fix.pr_fix_reason":                  "stale",
			"step.repair_conflicts.pr_fix_requires_human": "false",
			"step.repair_conflicts.pr_fix_reason":         "",
			"step.repair_conflicts.output":                "SYBRA_PR_FIX_RESULT: human-required\n",
		},
	}

	gotHuman, gotReason := prFixRequiresHuman(wf)
	if gotHuman {
		t.Fatalf("requiresHuman = true, reason %q; want explicit false from latest agent step", gotReason)
	}
}

func TestAdvanceStep_PRFixHumanRequiredUsesUntruncatedOutput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "pr-fix-route-test",
		Name: "PR fix route test",
		Steps: []Step{
			{
				ID:   "fix",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:   "pr-fix",
					Prompt: "fix",
				},
				Next: []Transition{{GoTo: "route_pr_fix_result"}},
			},
			{
				ID:   "route_pr_fix_result",
				Type: StepRoutePRFixResult,
				Next: []Transition{
					{
						When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"},
						GoTo: "",
					},
					{GoTo: ""},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	if err := engine.StartWorkflow("t1", "pr-fix-route-test"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	longOutput := strings.Repeat("progress details\n", 400) +
		"SYBRA_PR_FIX_RESULT: human-required\n" +
		"SYBRA_PR_FIX_REASON: 5 conflicts exceed the limit\n"
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "fix",
		Status:  "completed",
		Output:  longOutput,
		AgentID: "agent-1",
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); reason != "5 conflicts exceed the limit" {
		t.Errorf("reason = %q, want sentinel reason", reason)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %+v, want completed", got.Workflow)
	}
}
