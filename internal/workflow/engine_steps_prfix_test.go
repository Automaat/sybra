package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
)

// scriptedPRStateFetcher returns a fixed state or error for every probe,
// regardless of repo/number, so tests can pin the "remote already resolved"
// signal without shelling out to `gh`.
type scriptedPRStateFetcher struct {
	state github.PRState
	err   error
}

func (f scriptedPRStateFetcher) FetchPRState(string, int) (github.PRState, error) {
	return f.state, f.err
}

func TestClassifyPRFixResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		output          string
		wantVerdict     PRFixVerdict
		wantReason      string
		wantEmptyReason bool
	}{
		{
			name: "sentinel human required with reason",
			output: "Aborted rebase.\nSYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: 5 conflicting files exceed the auto-resolve limit\n",
			wantVerdict: PRFixHuman,
			wantReason:  "5 conflicting files exceed the auto-resolve limit",
		},
		{
			name:        "sentinel continue",
			output:      "Pushed fixes.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "sentinel flake with reason",
			output: "The failing job also fails on main.\nSYBRA_PR_FIX_RESULT: flake\n" +
				"SYBRA_PR_FIX_REASON: e2e provisioning timeout, reproduces on base\n",
			wantVerdict: PRFixFlake,
			wantReason:  "e2e provisioning timeout, reproduces on base",
		},
		{
			name:        "sentinel no-op alias maps to flake",
			output:      "Nothing to change.\nSYBRA_PR_FIX_RESULT: no-op\n",
			wantVerdict: PRFixFlake,
		},
		{
			name:            "flake without a reason sentinel reports no reason",
			output:          "Nothing to change.\nSYBRA_PR_FIX_RESULT: flake\n",
			wantVerdict:     PRFixFlake,
			wantEmptyReason: true,
		},
		{
			name: "legacy conflict abort text",
			output: "The rebase produced 5 conflicting files, which exceeds the limit of 3. " +
				"As instructed, I ran git rebase --abort. This task requires human review.",
			wantVerdict: PRFixHuman,
			wantReason:  "pr-fix agent requested human review: The rebase produced 5 conflicting files, which exceeds the limit of 3. As instructed, I ran git rebase --abort. This task requires human review.",
		},
		{
			name:        "negative human phrase",
			output:      "The conflict is resolved; no human review is required.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "last sentinel wins",
			output: "Example contract:\nSYBRA_PR_FIX_RESULT: human-required\n\nActual result:\n" +
				"SYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "flake sentinel beats an earlier contract echo",
			output: "Contract says:\nSYBRA_PR_FIX_RESULT: human-required\n\nActual:\n" +
				"SYBRA_PR_FIX_RESULT: flake\n",
			wantVerdict: PRFixFlake,
		},
		{
			name: "last reason wins",
			output: "SYBRA_PR_FIX_REASON: example only\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: real blocker\n",
			wantVerdict: PRFixHuman,
			wantReason:  "real blocker",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVerdict, gotReason := classifyPRFixResult(tc.output)
			if gotVerdict != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v", gotVerdict, tc.wantVerdict)
			}
			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
			if tc.wantEmptyReason && gotReason != "" {
				t.Errorf("reason = %q, want empty; a non-human verdict must not inherit the human-required default text", gotReason)
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

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1178})
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

// A flake verdict must not park a human and must not reach verify_commits,
// which would fail the task for the missing commit the honest answer implies.
func TestExecRoutePRFixResult_FlakeRoutesToInReviewWithoutCommit(t *testing.T) {
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
			Output:    "Same job fails on base.\nSYBRA_PR_FIX_RESULT: flake\nSYBRA_PR_FIX_REASON: e2e provisioning timeout, reproduces on base\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1178, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1178})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want the flake message so verify_commits is skipped")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-review" {
		t.Fatalf("status = %q, want in-review (a flake must never park a human)", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "reproduces on base") {
		t.Errorf("reason = %q, want the agent's flake evidence", reason)
	}
}

// A review-hold park must beat a flake sentinel: the drafted pending review
// still needs a human to submit it regardless of why CI failed.
func TestExecRoutePRFixResult_ReviewHoldParkBeatsFlake(t *testing.T) {
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
			Output:    "SYBRA_PR_FIX_RESULT: flake\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (review hold must beat a flake sentinel)", got.Status)
	}
}

// TestExecRoutePRFixResult_ReProbesResolvedRemotePR pins the bug report's
// scenario: the pr-fix agent correctly declined to push because its local
// worktree was stale/diverged, but the remote PR is already green and
// mergeable (an external bot fixed it out from under the task). The step
// must re-probe and route to in-review instead of parking human-required.
func TestExecRoutePRFixResult_ReProbesResolvedRemotePR(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}})
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Local worktree is diverged from origin; declining to push.\nSYBRA_PR_FIX_RESULT: human-required\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1178, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1178})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want resolved-on-remote message")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-review" {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
}

// TestExecRoutePRFixResult_ReviewHoldParkIgnoresResolvedRemotePR asserts the
// re-probe never overrides a review-hold park: that park exists because a
// pending review draft needs a human to submit it, which is orthogonal to
// whether CI is green.
func TestExecRoutePRFixResult_ReviewHoldParkIgnoresResolvedRemotePR(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}})
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Pushed the fix.\nSYBRA_PR_FIX_RESULT: continue",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1446, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1446})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue; review-hold park must force human-required")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (review-hold must not be waved through)", got.Status)
	}
}

func TestExecRoutePRFixResult_ReviewHoldParkWinsOverContinue(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	// The agent pushed in review-hold push mode and reported `continue`; the
	// deterministic park var must still route the task to human-required so the
	// drafted pending review isn't silently left unsubmitted.
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Pushed the fix.\nSYBRA_PR_FIX_RESULT: continue",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1446, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1446})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue; review-hold park must force human-required")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
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

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixContinue {
		t.Fatalf("verdict = %v, reason %q; want continue from latest agent step's explicit false var", gotVerdict, gotReason)
	}
}

func TestPRFixVerdict_VerdictVarWinsOverLegacyBool(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			Output:  "SYBRA_PR_FIX_RESULT: flake\n",
			AgentID: "agent-1",
		}},
		Variables: map[string]string{
			"step.fix." + PRFixVerdictVar:    string(PRFixFlake),
			"step.fix.pr_fix_requires_human": "false",
			"step.fix.pr_fix_reason":         "unrelated e2e provisioning failure",
		},
	}

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixFlake {
		t.Fatalf("verdict = %v, want %v", gotVerdict, PRFixFlake)
	}
	if gotReason != "unrelated e2e provisioning failure" {
		t.Errorf("reason = %q, want the flake reason", gotReason)
	}
}

func TestPRFixVerdict_LegacyExecutionWithoutVerdictVarParksHuman(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			Output:  "SYBRA_PR_FIX_RESULT: human-required\n",
			AgentID: "agent-1",
		}},
		Variables: map[string]string{
			"step.fix.pr_fix_requires_human": "true",
			"step.fix.pr_fix_reason":         "needs a human",
		},
	}

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixHuman {
		t.Fatalf("verdict = %v, want %v for a pre-flake execution", gotVerdict, PRFixHuman)
	}
	if gotReason != "needs a human" {
		t.Errorf("reason = %q, want the legacy reason", gotReason)
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
