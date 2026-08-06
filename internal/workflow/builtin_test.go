package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuiltinSimpleTaskPlan_ApprovalStartsImplementation(t *testing.T) {
	t.Parallel()

	plan := mustBuiltinDefinition(t, "simple-task-plan")
	review := plan.StepByID("review_plan")
	if review == nil {
		t.Fatal("review_plan step not found in simple-task-plan")
	}
	got, err := ResolveTransition(review.Next, map[string]string{"vars.human_action": "approve"})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "set_in_progress_and_end" {
		t.Fatalf("approval next = %q, want set_in_progress_and_end", got)
	}
	step := plan.StepByID(got)
	if step == nil || step.Type != StepSetStatus || step.Config.Status != "in-progress" {
		t.Fatalf("approval handoff = %+v, want set_status in-progress", step)
	}
}

func TestBuiltinWorkflowModelRouting(t *testing.T) {
	t.Parallel()

	plan := mustBuiltinDefinition(t, "simple-task-plan")
	critique := plan.StepByID("critique_plan")
	if critique == nil {
		t.Fatal("critique_plan step not found in simple-task-plan")
	}
	if got := critique.Config.Model; got != "supercheap" {
		t.Fatalf("critique_plan model = %q, want supercheap", got)
	}

	testingDef := mustBuiltinDefinition(t, "testing-task")
	runTest := testingDef.StepByID("run_test")
	if runTest == nil {
		t.Fatal("run_test step not found in testing-task")
	}
	if got := runTest.Config.Model; got != "supercheap" {
		t.Fatalf("run_test model = %q, want supercheap", got)
	}

	implement := mustBuiltinDefinition(t, "simple-task-implement").StepByID("implement")
	if implement == nil {
		t.Fatal("implement step not found in simple-task-implement")
	}
	wantTemplate := `{{if getvar .Vars "verify_retry_model"}}{{getvar .Vars "verify_retry_model"}}{{else}}cheap{{end}}`
	if got := strings.TrimSpace(implement.Config.Model); got != wantTemplate {
		t.Fatalf("implement model = %q, want %q", got, wantTemplate)
	}
	if !strings.Contains(implement.Config.Model, verifyRetryModelVar) {
		t.Fatalf("implement model = %q, want %q reference", implement.Config.Model, verifyRetryModelVar)
	}

	bestOfN := mustBuiltinDefinition(t, "simple-task-best-of-n-implement").StepByID("attempts")
	if bestOfN == nil {
		t.Fatal("attempts step not found in simple-task-best-of-n-implement")
	}
	if got := bestOfN.Config.Model; got != "cheap" {
		t.Fatalf("attempts model = %q, want cheap", got)
	}
}

// TestBuiltinSimpleTask_MaybeCritiqueReplanSkip locks the behavior that
// critique_plan runs only on the first plan pass. On replan (after a human
// reject), `vars.step.review_plan.output` exists, so maybe_critique must
// route directly to review_plan and spare the critic a second run on a
// plan the human already eyeballed once. Substring-checking tag "nocritic"
// is still honored as an opt-out on the first pass.
func TestBuiltinSimpleTask_MaybeCritiqueReplanSkip(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
		return
	}
	step := simple.StepByID("maybe_critique")
	if step == nil {
		t.Fatal("maybe_critique step not found in simple-task-plan")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "first_pass_runs_critique",
			fields: map[string]string{
				"task.tags": "backend,feature",
			},
			want: "critique_plan",
		},
		{
			name: "nocritic_tag_skips_critique",
			fields: map[string]string{
				"task.tags": "backend,nocritic",
			},
			want: "review_plan",
		},
		{
			name: "replan_skips_critique_even_without_nocritic",
			fields: map[string]string{
				"task.tags":                    "backend,feature",
				"vars.step.review_plan.output": "reject",
				"vars.human_action":            "reject",
			},
			want: "review_plan",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuiltinSimpleTask_MissingCritiqueSkipsToHumanReview(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
		return
	}
	step := simple.StepByID("require_plan_critique")
	if step == nil {
		t.Fatal("require_plan_critique step not found in simple-task-plan")
		return
	}
	if !step.Config.AllowMissing {
		t.Fatal("require_plan_critique must soft-fail when the critic produces no sidecar")
	}
	got, err := ResolveTransition(step.Next, map[string]string{
		"vars.step.require_plan_critique.output": "plan critique missing — upstream agent step completed without writing its sidecar — skipped (non-fatal)",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "review_plan" {
		t.Fatalf("missing critique next = %q, want review_plan", got)
	}
}

// TestBuiltinSimpleTask_PresentCritiqueRoutesThroughFlagStep pins that a
// present (non-skipped) plan critique routes through flag_plan_critique_verdict
// before review_plan, not directly to review_plan. A regression that
// re-points require_plan_critique's default transition straight at
// review_plan would silently disable the whole REFINE/REJECT-visibility
// feature (issue #2222) while still passing every unit test against
// execFlagPlanCritique itself, since those call it directly without going
// through this wiring.
func TestBuiltinSimpleTask_PresentCritiqueRoutesThroughFlagStep(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
		return
	}

	require := simple.StepByID("require_plan_critique")
	if require == nil {
		t.Fatal("require_plan_critique step not found in simple-task-plan")
		return
	}
	got, err := ResolveTransition(require.Next, map[string]string{
		"vars.step.require_plan_critique.output": "plan critique present",
		"task.status":                            "planning",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "flag_plan_critique_verdict" {
		t.Fatalf("present critique next = %q, want flag_plan_critique_verdict", got)
	}

	flag := simple.StepByID("flag_plan_critique_verdict")
	if flag == nil {
		t.Fatal("flag_plan_critique_verdict step not found in simple-task-plan")
		return
	}
	if flag.Type != StepFlagPlanCritique {
		t.Fatalf("flag_plan_critique_verdict type = %q, want %q", flag.Type, StepFlagPlanCritique)
	}
	got, err = ResolveTransition(flag.Next, map[string]string{})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "review_plan" {
		t.Fatalf("flag_plan_critique_verdict next = %q, want review_plan", got)
	}
}

// TestBuiltinSimpleTask_ReplanCapEscalatesAfterThreeRejects locks the replan
// iteration cap: task.replan_count is start_replan's own step-history count
// as of the current reject, so 0/1/2 still have budget for another full
// opus replan cycle, and 3+ hands the task to a human instead of burning a
// 4th opus run on a plan the human keeps rejecting.
func TestBuiltinSimpleTask_ReplanCapEscalatesAfterThreeRejects(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("check_replan_cap")
	if step == nil {
		t.Fatal("check_replan_cap step not found in simple-task-plan")
	}

	cases := []struct {
		name        string
		replanCount string
		want        string
	}{
		{name: "first_reject_under_cap", replanCount: "0", want: "start_replan"},
		{name: "second_reject_under_cap", replanCount: "1", want: "start_replan"},
		{name: "third_reject_under_cap", replanCount: "2", want: "start_replan"},
		{name: "fourth_reject_hits_cap", replanCount: "3", want: "replan_cap_exceeded"},
		{name: "well_past_cap", replanCount: "9", want: "replan_cap_exceeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, map[string]string{
				"task.replan_count": tc.replanCount,
			})
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}

	exceeded := simple.StepByID("replan_cap_exceeded")
	if exceeded == nil {
		t.Fatal("replan_cap_exceeded step not found in simple-task-plan")
		return
	}
	if exceeded.Config.Status != "human-required" {
		t.Errorf("replan_cap_exceeded status = %q, want human-required", exceeded.Config.Status)
	}
}

// TestBuiltinSimpleTaskPlan_CritiqueDoesNotTriggerReplan locks the ungated
// critique contract (#2152): the critic's verdict is advisory context for the
// human gate, never a router. The old route_critique_verdict step gated on the
// literal "Plan Review: APPROVE", a format the plan-critic skill never emits
// (it writes "# Plan Review" and "## Verdict: <X>" on separate lines), so every
// critiqued plan fell through to a second full-price opus address_critique run.
// No plan-role agent may be reachable from the critique path.
func TestBuiltinSimpleTaskPlan_CritiqueDoesNotTriggerReplan(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
		return
	}

	// No step may branch on the critique's content: a verdict-shaped gate is
	// exactly what silently forced the rerun, since the critic's real output
	// never matched the literal the old gate tested for.
	for _, s := range simple.Steps {
		for _, n := range s.Next {
			if n.When != nil && n.When.Field == "task.plan_critique" {
				t.Errorf("step %q branches on task.plan_critique (%q %q) — the critique is advisory context, not a router",
					s.ID, n.When.Operator, n.When.Value)
			}
		}
	}

	// The rework path is the human reject loop only — no plan-role agent step
	// may remain besides the initial plan.
	var planRoleSteps []string
	for _, s := range simple.Steps {
		if s.Type == StepRunAgent && s.Config.Role == "plan" {
			planRoleSteps = append(planRoleSteps, s.ID)
		}
	}
	if len(planRoleSteps) != 1 || planRoleSteps[0] != "plan" {
		t.Errorf("plan-role agent steps = %v, want exactly [plan]", planRoleSteps)
	}
}

// The planning pipeline runs autonomously with no human attached. Every
// run_agent step must therefore be headless: an interactive-mode agent is
// skipped by the watchdog (internal/watchdog only supervises ag.Mode ==
// "headless"), so a hang leaves the workflow waiting forever with no stall
// recovery — the failure mode that wedged task 3c1c4f12 in `planning`.
func TestBuiltinSimpleTaskPlan_AgentStepsAreHeadless(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}

	var runAgentSteps int
	for i := range simple.Steps {
		step := &simple.Steps[i]
		if step.Type != StepRunAgent {
			continue
		}
		runAgentSteps++
		if step.Config.Mode != "headless" {
			t.Errorf("run_agent step %q mode = %q, want headless (autonomous planning agents must stay under watchdog supervision)", step.ID, step.Config.Mode)
		}
	}
	if runAgentSteps == 0 {
		t.Fatal("expected simple-task-plan to contain run_agent steps")
	}
}

func TestBuiltinSimpleTaskImplement_UsesCompactTaskView(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin definition not found")
		return
	}
	step := impl.StepByID("implement")
	if step == nil {
		t.Fatal("implement step not found in simple-task-implement")
		return
	}
	if !strings.Contains(step.Config.Prompt, "sybra-cli get --compact {{.Task.ID}}") {
		t.Fatalf("implement prompt must use compact task view, got:\n%s", step.Config.Prompt)
	}
}

// TestBuiltinSimpleTaskImplement_DisclosesDownstreamVerification pins the
// initial prompt naming the authoritative deterministic checks that run
// after the agent pushes (codegen_gate, detect_tampering, verify_checks), so
// the agent iterates with focused checks instead of repeating the full
// build/lint/test suite locally. See #2020.
func TestBuiltinSimpleTaskImplement_DisclosesDownstreamVerification(t *testing.T) {
	t.Parallel()

	impl := mustBuiltinDefinition(t, "simple-task-implement")
	step := impl.StepByID("implement")
	if step == nil {
		t.Fatal("implement step not found in simple-task-implement")
	}
	prompt := step.Config.Prompt

	for _, want := range []string{
		"codegen/format gate",
		"tamper detector",
		"checks.verify` suite is the authoritative pass/fail gate",
		"use focused checks",
		"do not repeatedly run the full build/lint/test suite",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("implement prompt must disclose downstream verification contract, missing %q, got:\n%s", want, prompt)
		}
	}

	// The contract must sit ahead of every failure-specific/retry block so a
	// retried implementation always sees it before that context. Pin it
	// against the earliest such block, not just the verify reask note —
	// currenttestfailures and acceptanceledger both precede verify_reask_note,
	// so checking only the latter would let an edit slip the contract below
	// them while staying green.
	contractIdx := strings.Index(prompt, "Downstream, after you push")
	if contractIdx < 0 {
		t.Fatalf("downstream verification contract not found in implement prompt, got:\n%s", prompt)
	}
	for _, block := range []string{
		"{{- if currenttestfailures .Task.CurrentTestFailures}}",
		"{{- if acceptanceledger .Task.AcceptanceLedger}}",
		`{{getvar .Vars "verify_reask_note"}}`,
		`{{getvar .Vars "watchdog_reask_note"}}`,
	} {
		blockIdx := strings.Index(prompt, block)
		if blockIdx < 0 {
			t.Fatalf("expected retry/failure block %q not found in implement prompt", block)
		}
		if contractIdx > blockIdx {
			t.Fatalf("downstream verification contract must precede retry/failure block %q, contractIdx=%d blockIdx=%d", block, contractIdx, blockIdx)
		}
	}
}

func TestBuiltinSimpleTaskPlan_RewardHackingRetryNotesReachAgents(t *testing.T) {
	t.Parallel()

	plan := mustBuiltinDefinition(t, "simple-task-plan")
	for _, stepID := range []string{"plan", "critique_plan"} {
		step := plan.StepByID(stepID)
		if step == nil {
			t.Fatalf("%s step not found in simple-task-plan", stepID)
		}
		for _, want := range []string{
			`{{- if getvar .Vars "watchdog_reask_note"}}`,
			`{{getvar .Vars "watchdog_reask_note"}}`,
		} {
			if !strings.Contains(step.Config.Prompt, want) {
				t.Fatalf("%s prompt must include reward-hacking retry note block %q, got:\n%s", stepID, want, step.Config.Prompt)
			}
		}
	}
}

func TestBuiltinPromptLabAuthor_OwnsPromptLabImplementation(t *testing.T) {
	t.Parallel()

	impl := mustBuiltinDefinition(t, "simple-task-implement")
	hasPromptLabExclusion := false
	for _, cond := range impl.Trigger.Conditions {
		if cond.Field == "task.tags" && cond.Operator == "not_contains" && cond.Value == "prompt-lab-proposal" {
			hasPromptLabExclusion = true
			break
		}
	}
	if !hasPromptLabExclusion {
		t.Fatal("simple-task-implement must exclude prompt-lab-proposal tasks")
	}

	promptLab := mustBuiltinDefinition(t, "prompt-lab-author")
	if promptLab.Trigger.On != "task.status_changed" || promptLab.Trigger.Priority <= impl.Trigger.Priority {
		t.Fatalf("prompt-lab-author trigger = %+v, want higher-priority task.status_changed trigger", promptLab.Trigger)
	}
	if !slices.ContainsFunc(promptLab.Trigger.Conditions, func(c Condition) bool {
		return c.Field == "task.tags" && c.Operator == "contains" && c.Value == "prompt-lab-proposal"
	}) {
		t.Fatalf("prompt-lab-author conditions = %+v, want prompt-lab-proposal tag match", promptLab.Trigger.Conditions)
	}
	step := promptLab.StepByID("author_variant")
	if step == nil {
		t.Fatal("prompt-lab-author author_variant step not found")
	}
	if step.Type != StepRunAgent || !step.Config.NeedsWorktree || step.Config.Mode != "headless" {
		t.Fatalf("author_variant step = %+v, want headless run_agent with worktree", step)
	}
	if !strings.Contains(step.Config.Prompt, "evaluation offline run") || !strings.Contains(step.Config.Prompt, "evaluation offline gate") {
		t.Fatalf("author_variant prompt must require offline eval run+gate, got:\n%s", step.Config.Prompt)
	}
}

// TestBuiltinSimpleTaskImplement_CodegenPrecedesValidation pins the
// implementation workflow ordering: verify_commits flows into codegen_gate,
// then focused_checks, which must still run before detect_tampering and
// verify_checks so downstream review/testing validate the final committed
// branch content.
func TestBuiltinSimpleTaskImplement_CodegenPrecedesValidation(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin definition not found")
	}
	verifyCommits := impl.StepByID("verify_commits")
	if verifyCommits == nil {
		t.Fatal("verify_commits step not found in simple-task-implement")
	}

	if got, err := ResolveTransition(verifyCommits.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("verify_commits human-required goto = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(verifyCommits.Next, map[string]string{"task.status": "done"}); err != nil || got != "" {
		t.Fatalf("verify_commits done goto = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(verifyCommits.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "codegen_gate" {
		t.Fatalf("verify_commits clean goto = %q, err=%v; want codegen_gate", got, err)
	}

	focused := impl.StepByID("focused_checks")
	if focused == nil {
		t.Fatal("focused_checks step not found in simple-task-implement")
	}
	if focused.Type != StepFocusedChecks {
		t.Fatalf("focused_checks type = %q, want %q", focused.Type, StepFocusedChecks)
	}
	if got, err := ResolveTransition(focused.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("focused_checks human-required goto = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(focused.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "detect_tampering" {
		t.Fatalf("focused_checks clean goto = %q, err=%v; want detect_tampering", got, err)
	}

	codegen := impl.StepByID("codegen_gate")
	if codegen == nil {
		t.Fatal("codegen_gate step not found in simple-task-implement")
	}
	if codegen.Type != StepCodegenGate {
		t.Fatalf("codegen_gate type = %q, want %q", codegen.Type, StepCodegenGate)
	}
	if got, err := ResolveTransition(codegen.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("codegen_gate human-required goto = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(codegen.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "focused_checks" {
		t.Fatalf("codegen_gate clean goto = %q, err=%v; want focused_checks", got, err)
	}

	detect := impl.StepByID("detect_tampering")
	if detect == nil {
		t.Fatal("detect_tampering step not found in simple-task-implement")
	}
	if got, err := ResolveTransition(detect.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "verify_checks" {
		t.Fatalf("detect_tampering clean goto = %q, err=%v; want verify_checks", got, err)
	}
}

// TestBuiltinSimpleTaskImplement_ExistingPRSkipsReadyReview pins the fix for
// a re-fix cycle orphaning at ready-review: simple-task-review's own trigger
// refuses pr_number != "", so verify_checks must route a PR-having task to
// in-review directly rather than a status nothing dispatches.
func TestBuiltinSimpleTaskImplement_ExistingPRSkipsReadyReview(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin definition not found")
	}
	verifyChecks := impl.StepByID("verify_checks")
	if verifyChecks == nil {
		t.Fatal("verify_checks step not found in simple-task-implement")
	}

	if got, err := ResolveTransition(verifyChecks.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "set_ready_review" {
		t.Fatalf("verify_checks no-PR goto = %q, err=%v; want set_ready_review", got, err)
	}
	if got, err := ResolveTransition(verifyChecks.Next, map[string]string{"task.status": "in-progress", "task.pr_number": "17478"}); err != nil || got != "set_ready_pr_existing" {
		t.Fatalf("verify_checks existing-PR goto = %q, err=%v; want set_ready_pr_existing", got, err)
	}
	if got, err := ResolveTransition(verifyChecks.Next, map[string]string{"task.status": "blocked", "task.pr_number": "17478"}); err != nil || got != "" {
		t.Fatalf("verify_checks blocked goto = %q, err=%v; want end (wins over PR routing)", got, err)
	}
	if got, err := ResolveTransition(verifyChecks.Next, map[string]string{"task.status": "human-required", "task.pr_number": "17478"}); err != nil || got != "" {
		t.Fatalf("verify_checks human-required goto = %q, err=%v; want end (wins over PR routing)", got, err)
	}

	existingPR := impl.StepByID("set_ready_pr_existing")
	if existingPR == nil {
		t.Fatal("set_ready_pr_existing step not found in simple-task-implement")
	}
	if existingPR.Type != StepSetStatus || existingPR.Config.Status != "in-review" {
		t.Fatalf("set_ready_pr_existing = type %q status %q; want set_status in-review", existingPR.Type, existingPR.Config.Status)
	}
}

// TestBuiltinSimpleTask_TriageNoplanRouting locks the noplan escape hatch in
// the triage step's transition table: a noplan tag routes straight to
// implement (winning over a planning status), terminal statuses still win over
// noplan, and the absence of noplan preserves the normal plan/implement split.
func TestBuiltinSimpleTask_TriageNoplanRouting(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("triage")
	if step == nil {
		t.Fatal("triage step not found in simple-task-plan")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "planning_without_noplan_goes_to_plan",
			fields: map[string]string{"task.status": "planning", "task.tags": "backend,feature"},
			want:   "plan",
		},
		{
			name:   "noplan_skips_planning_even_when_status_planning",
			fields: map[string]string{"task.status": "planning", "task.tags": "backend,noplan"},
			want:   "set_in_progress_and_end",
		},
		{
			name:   "todo_without_noplan_hands_off_to_implement",
			fields: map[string]string{"task.status": "todo", "task.tags": "backend"},
			want:   "set_in_progress_and_end",
		},
		{
			name:   "terminal_status_wins_over_noplan",
			fields: map[string]string{"task.status": "done", "task.tags": "backend,noplan"},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuiltinSimpleTaskReview_MaybeReviewTrivialRouting locks the review
// entry gate in simple-task-review: a trivial tag routes straight to
// done_review (skipping the code-review agents), same as a CLEAN-reviewed
// task, while noreview and the normal path are unaffected. A persisted
// NEEDS_FIXES sidecar must override task.reviewed so the review-cap →
// testing → reimplementation loop cannot skip the next code review.
func TestBuiltinSimpleTaskReview_MaybeReviewTrivialRouting(t *testing.T) {
	t.Parallel()

	review := mustBuiltinDefinition(t, "simple-task-review")
	step := review.StepByID("maybe_review")
	if step == nil {
		t.Fatal("maybe_review step not found in simple-task-review")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "no_escape_hatch_goes_to_triage_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,feature"},
			want:   "triage_review",
		},
		{
			name:   "trivial_skips_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,trivial"},
			want:   "done_review",
		},
		{
			name:   "noreview_skips_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,noreview"},
			want:   "done_review",
		},
		{
			name:   "already_reviewed_wins_over_normal_path",
			fields: map[string]string{"task.reviewed": "true", "task.tags": "backend,feature"},
			want:   "done_review",
		},
		{
			name: "needs_fixes_review_overrides_reviewed_flag",
			fields: map[string]string{
				"task.reviewed":    "true",
				"task.tags":        "backend,feature",
				"task.code_review": "Review Verdict: NEEDS_FIXES\n\nfoo.go:12: nil deref risk.\n",
			},
			want: "triage_review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuiltinSimpleTaskReview_RouteReviewVerdictSkipsFixReviewOnClean locks
// the #1524 fix: a clean review (zero actionable findings) routes straight
// to done_review, skipping the fix_review agent entirely.
func TestBuiltinSimpleTaskReview_RouteReviewVerdictSkipsFixReviewOnClean(t *testing.T) {
	t.Parallel()

	review := mustBuiltinDefinition(t, "simple-task-review")
	step := review.StepByID("route_review_verdict")
	if step == nil {
		t.Fatal("route_review_verdict step not found in simple-task-review")
	}

	cases := []struct {
		name       string
		codeReview string
		want       string
	}{
		{
			name:       "clean_skips_fix_review",
			codeReview: "Review Verdict: CLEAN\n\nNo actionable findings.\n",
			want:       "done_review",
		},
		{
			name:       "needs_fixes_routes_to_fix_review",
			codeReview: "Review Verdict: NEEDS_FIXES\n\nfoo.go:12: nil deref risk.\n",
			want:       "fix_review",
		},
		{
			name: "needs_fixes_echoing_clean_marker_routes_to_fix_review",
			codeReview: "Review Verdict: NEEDS_FIXES\n\n" +
				"This is not a Review Verdict: CLEAN pass; see the finding below.\n\n" +
				"foo.go:12: nil deref risk.\n",
			want: "fix_review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, map[string]string{
				"task.code_review": tc.codeReview,
			})
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuiltinDefinitions(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("expected non-empty builtin definitions")
	}
	for _, d := range defs {
		if d.ID == "" {
			t.Errorf("builtin definition has empty ID: %+v", d)
		}
		if !d.Builtin {
			t.Errorf("builtin definition %q has Builtin=false", d.ID)
		}
	}
}

func TestBuiltinDefinitions_Valid(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for _, d := range defs {
		t.Run(d.ID, func(t *testing.T) {
			t.Parallel()
			if err := d.Validate(); err != nil {
				t.Errorf("Validate() error for %q: %v", d.ID, err)
			}
		})
	}
}

func TestBuiltinPRFix_RoutesAgentHumanRequiredBeforePRRelink(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var prfix *Definition
	for i := range defs {
		if defs[i].ID == "pr-fix" {
			prfix = &defs[i]
			break
		}
	}
	if prfix == nil {
		t.Fatal("pr-fix builtin definition not found")
	}
	fix := prfix.StepByID("fix")
	if fix == nil {
		t.Fatal("fix step missing from pr-fix")
		return
	}
	if got, err := ResolveTransition(fix.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "route_pr_fix_result" {
		t.Fatalf("fix next = %q, err=%v; want route_pr_fix_result", got, err)
	}
	route := prfix.StepByID("route_pr_fix_result")
	if route == nil {
		t.Fatal("route_pr_fix_result step missing from pr-fix")
		return
	}
	if route.Type != StepRoutePRFixResult {
		t.Fatalf("route_pr_fix_result type = %q, want %q", route.Type, StepRoutePRFixResult)
	}
	if got, err := ResolveTransition(route.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("human-required route next = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(route.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "verify_commits" {
		t.Fatalf("default route next = %q, err=%v; want verify_commits", got, err)
	}
}

// TestBuiltinPRFix_TestFixEligibleRoutesBeforeHumanRequired pins that
// route_pr_fix_result's pr_fix_test_fix_eligible branch is checked before
// its task.status==human-required branch — otherwise the eligibility var
// would never get a chance to redirect to test_fix, since a real eligible
// completion always has task.status still at in-progress (unset) rather
// than human-required at this point, but a stale/misordered condition list
// checked in the wrong order would silently never route there in practice
// once a future edit changes what status looks like at this step.
func TestBuiltinPRFix_TestFixEligibleRoutesBeforeHumanRequired(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var prfix *Definition
	for i := range defs {
		if defs[i].ID == "pr-fix" {
			prfix = &defs[i]
			break
		}
	}
	if prfix == nil {
		t.Fatal("pr-fix builtin definition not found")
	}
	route := prfix.StepByID("route_pr_fix_result")
	if route == nil {
		t.Fatal("route_pr_fix_result step missing from pr-fix")
		return
	}
	got, err := ResolveTransition(route.Next, map[string]string{
		"vars.step.route_pr_fix_result.pr_fix_test_fix_eligible": "true",
		"task.status": "in-progress",
	})
	if err != nil || got != "test_fix" {
		t.Fatalf("eligible route next = %q, err=%v; want test_fix", got, err)
	}

	testFix := prfix.StepByID("test_fix")
	if testFix == nil {
		t.Fatal("test_fix step missing from pr-fix")
		return
	}
	if testFix.Type != StepRunAgent {
		t.Fatalf("test_fix type = %q, want %q", testFix.Type, StepRunAgent)
	}
	if testFix.Config.Role != "test-fix" {
		t.Fatalf("test_fix role = %q, want test-fix", testFix.Config.Role)
	}
	if got, err := ResolveTransition(testFix.Next, map[string]string{}); err != nil || got != "route_test_fix_result" {
		t.Fatalf("test_fix next = %q, err=%v; want route_test_fix_result", got, err)
	}

	routeTestFix := prfix.StepByID("route_test_fix_result")
	if routeTestFix == nil {
		t.Fatal("route_test_fix_result step missing from pr-fix")
		return
	}
	if routeTestFix.Type != StepRoutePRFixResult {
		t.Fatalf("route_test_fix_result type = %q, want %q", routeTestFix.Type, StepRoutePRFixResult)
	}
	// route_test_fix_result must have no eligibility branch of its own — a
	// human-required verdict here always parks, bounding the follow-up to
	// exactly one attempt.
	if got, err := ResolveTransition(routeTestFix.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("route_test_fix_result human-required next = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(routeTestFix.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "verify_commits" {
		t.Fatalf("route_test_fix_result default next = %q, err=%v; want verify_commits", got, err)
	}

	// A pure wiring assertion can't catch a template syntax typo or a wrong
	// getvar key, so render the real prompt string against realistic vars.
	rendered, err := RenderTemplate(testFix.Config.Prompt, TemplateContext{
		Task: TaskInfo{Branch: "chore/example-branch-1234abcd"},
		Vars: map[string]string{
			"step.fix.pr_fix_failing_tests": "pkg/a_test.go:1 TestA\npkg/b_test.go:2 TestB",
			"step.fix.pr_fix_reason":        "targeted tests still fail after the merge",
			// A stand-in for internal/sybra/review.PRFixResultContract
			// (that package imports this one, so it can't be referenced
			// directly here) — only its presence in the rendered output
			// matters for this test, not its exact wording.
			"pr_fix_result_contract": "SYBRA_PR_FIX_RESULT: <verdict>",
			// Stand-in for project.CommitSignFlags(ctx) — dispatchPRIssueWithOptions
			// (internal/sybra/review) computes the real value per-host so a
			// keyless host never gets a hardcoded -S it can't satisfy.
			"commit_sign_flags": "-s -S",
		},
	})
	if err != nil {
		t.Fatalf("render test_fix prompt: %v", err)
	}
	for _, want := range []string{
		"pkg/a_test.go:1 TestA",
		"pkg/b_test.go:2 TestB",
		"targeted tests still fail after the merge",
		"SYBRA_PR_FIX_RESULT",
		// Without explicit push instructions naming the branch, a test_fix
		// agent that fixes the tests but never pushes leaves the PR
		// unchanged while the workflow proceeds as if it succeeded.
		"git push",
		"chore/example-branch-1234abcd",
		"Do not force-push",
		// Commit-sign flags must come from the templated var, not a
		// hardcoded "-s -S" that fails on a keyless host.
		"git commit -s -S",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered test_fix prompt missing %q; got:\n%s", want, rendered)
		}
	}
	if strings.Contains(testFix.Config.Prompt, "-s -S") {
		t.Fatalf("test_fix prompt hardcodes commit sign flags instead of templating commit_sign_flags:\n%s", testFix.Config.Prompt)
	}
}

// TestBuiltinDefinitions_NeverInstructForcePush is the repo-wide acceptance
// probe for the "never force-push" invariant: no builtin workflow prompt may
// instruct an agent to force-push, under any spelling. Sybra's own process
// (project.PushSync) already refuses to force-push and instead returns
// ErrDivergedNeedsResolve for agent-driven recovery — this test guards the
// other force-push surface, the prompts Sybra authors for agents, so a new
// builtin workflow can't reintroduce a force-push instruction unnoticed.
func TestBuiltinDefinitions_NeverInstructForcePush(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	// Match actual git-push instructions, not advisory prose like
	// "Never force-push" or unrelated commands that may legitimately use
	// a --force flag.
	forbidden := []string{"--force-with-lease", "--force", " -f", "\t-f", "`-f`"}
	for _, def := range defs {
		for _, step := range def.Steps {
			prompt := step.Config.Prompt
			if prompt == "" {
				continue
			}
			for line := range strings.Lines(prompt) {
				if !strings.Contains(line, "git push") {
					continue
				}
				for _, term := range forbidden {
					if strings.Contains(line, term) {
						t.Fatalf("builtin %q step %q prompt instructs force-push (contains %q):\n%s", def.ID, step.ID, term, prompt)
					}
				}
			}
		}
	}
}

// TestBuiltinSimpleTaskPR_CreateAndPushAreDeterministic proves create_pr and
// push_existing_pr are the mechanized `create_pr`/`push_branch` step types,
// not an agent whose prompt hand-rolls fork-remote routing/never-force-push
// logic in shell. That routing now lives in project.HeadArg/PushSync (see
// TestHeadArg_WithFork/NoFork and TestPushSync_* in internal/project), and
// this test just guards against a regression back to a run_agent step.
func TestBuiltinSimpleTaskPR_CreateAndPushAreDeterministic(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-pr" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-pr builtin definition not found")
		return
	}

	createStep := simple.StepByID("create_pr")
	if createStep == nil {
		t.Fatal("create_pr step not found in simple-task-pr")
		return
	}
	if createStep.Type != StepCreatePR {
		t.Fatalf("create_pr step type = %q, want %q", createStep.Type, StepCreatePR)
	}
	if createStep.Config.Prompt != "" {
		t.Fatalf("create_pr step still carries an agent prompt: %q", createStep.Config.Prompt)
	}

	pushStep := simple.StepByID("push_existing_pr")
	if pushStep == nil {
		t.Fatal("push_existing_pr step not found in simple-task-pr")
		return
	}
	if pushStep.Type != StepPushBranch {
		t.Fatalf("push_existing_pr step type = %q, want %q", pushStep.Type, StepPushBranch)
	}
	if pushStep.Config.Prompt != "" {
		t.Fatalf("push_existing_pr step still carries an agent prompt: %q", pushStep.Config.Prompt)
	}
}

// TestBuiltinSimpleTaskReview_FixReviewPushesBeforeVerify guards the
// post-review-fix handoff. The fix-review agent commits locally and is
// intentionally told not to push, so the workflow must sync the branch through
// the deterministic push_branch step before verify_checks observes the final
// HEAD.
func TestBuiltinSimpleTaskReview_FixReviewPushesBeforeVerify(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-review builtin definition not found")
		return
	}

	fixStep := simple.StepByID("fix_review")
	if fixStep == nil {
		t.Fatal("fix_review step not found in simple-task-review")
		return
	}
	if fixStep.Type != StepRunAgent {
		t.Fatalf("fix_review step type = %q, want %q", fixStep.Type, StepRunAgent)
	}
	if strings.Contains(fixStep.Config.Prompt, "git push") {
		t.Fatalf("fix_review prompt asks the agent to push; push must be deterministic:\n%s", fixStep.Config.Prompt)
	}
	if len(fixStep.Next) != 1 || fixStep.Next[0].GoTo != "push_review_fix_branch" {
		t.Fatalf("fix_review next = %+v, want push_review_fix_branch", fixStep.Next)
	}

	pushStep := simple.StepByID("push_review_fix_branch")
	if pushStep == nil {
		t.Fatal("push_review_fix_branch step not found in simple-task-review")
		return
	}
	if pushStep.Type != StepPushBranch {
		t.Fatalf("push_review_fix_branch step type = %q, want %q", pushStep.Type, StepPushBranch)
	}
	if pushStep.Config.Prompt != "" {
		t.Fatalf("push_review_fix_branch step carries an agent prompt: %q", pushStep.Config.Prompt)
	}
	if len(pushStep.Next) == 0 || pushStep.Next[len(pushStep.Next)-1].GoTo != "verify_checks" {
		t.Fatalf("push_review_fix_branch next = %+v, want default goto verify_checks", pushStep.Next)
	}
}

// TestBuiltinSimpleTaskPR_NoCodegenAfterTesting guards the PR handoff
// workflow's non-mutating contract: after review/testing pass, the workflow
// may sync, push, create, and link a PR, but it must not run codegen_gate and
// create a new unreviewed commit.
func TestBuiltinSimpleTaskPR_NoCodegenAfterTesting(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-pr" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-pr builtin definition not found")
		return
	}

	// sync_branch runs first so require_evidence evaluates the exact HEAD that
	// will ship: a branch-moving sync must not slip an unverified merge/rebase
	// past the evidence gate.
	first := simple.FirstStep()
	if first == nil || first.Type != StepSyncBranch {
		t.Fatalf("FirstStep = %+v, want sync_branch first", first)
	}
	if len(first.Next) != 1 || first.Next[0].GoTo != "require_evidence" {
		t.Fatalf("sync_branch.Next = %+v, want unconditional goto require_evidence", first.Next)
	}

	evidenceStep := simple.StepByID("require_evidence")
	if evidenceStep == nil {
		t.Fatal("require_evidence step not found in simple-task-pr")
		return
	}
	if len(evidenceStep.Next) != 2 || evidenceStep.Next[len(evidenceStep.Next)-1].GoTo != "maybe_create_pr" {
		t.Fatalf("require_evidence.Next = %+v, want a fallback goto maybe_create_pr", evidenceStep.Next)
	}
	if step := simple.StepByID("codegen_gate"); step != nil {
		t.Fatalf("codegen_gate must not exist in simple-task-pr; got %+v", step)
	}

	// maybe_create_pr must still be the branch point covering both downstream
	// PR paths after the best-effort sync.
	guard := simple.StepByID("maybe_create_pr")
	if guard == nil {
		t.Fatal("maybe_create_pr step not found in simple-task-pr")
		return
	}
	var gotoTargets []string
	for _, n := range guard.Next {
		gotoTargets = append(gotoTargets, n.GoTo)
	}
	if !slices.Contains(gotoTargets, "push_existing_pr") || !slices.Contains(gotoTargets, "create_pr") {
		t.Fatalf("maybe_create_pr.Next targets = %v, want both push_existing_pr and create_pr", gotoTargets)
	}
}

func TestBuiltinDefinitions_NoDuplicateIDs(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	seen := make(map[string]bool)
	for _, d := range defs {
		if seen[d.ID] {
			t.Errorf("duplicate builtin ID: %q", d.ID)
		}
		seen[d.ID] = true
	}
}

func TestSyncBuiltins(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != len(defs) {
		t.Errorf("store has %d workflows, want %d", len(listed), len(defs))
	}
}

func TestSyncBuiltins_NoOverwrite(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Save a modified version of the first builtin.
	modified := defs[0]
	modified.Name = "user-modified"
	modified.Builtin = false // simulate user edit
	if err := store.Save(modified); err != nil {
		t.Fatalf("Save modified: %v", err)
	}

	// SyncBuiltins must not overwrite.
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(modified.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "user-modified" {
		t.Errorf("SyncBuiltins overwrote user modification: got %q, want %q", got.Name, "user-modified")
	}
}

// TestSyncBuiltins_OverwriteStaleBuiltin locks the drift-repair behavior:
// a stored workflow still marked Builtin=true must get replaced when its
// content diverges from the embedded copy. Matches the historical pr-fix
// drift that left `operator: contains` on a scalar enum field and silently
// broke all auto-fix dispatch.
func TestSyncBuiltins_OverwriteStaleBuiltin(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Simulate drift: save with Builtin=true but a stale name.
	stale := defs[0]
	stale.Name = "stale-drifted-name"
	stale.Builtin = true
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != defs[0].Name {
		t.Errorf("SyncBuiltins did not repair stale builtin: got %q, want %q",
			got.Name, defs[0].Name)
	}
}

func TestSyncBuiltins_PrunesObsoleteBuiltin(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	obsolete := newTestDef("obsolete-builtin-sync-test")
	obsolete.Builtin = true
	if err := store.Save(obsolete); err != nil {
		t.Fatalf("Save obsolete: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if _, err := store.Get(obsolete.ID); err == nil {
		t.Fatalf("obsolete builtin %q still exists", obsolete.ID)
	}
}

func TestSyncBuiltins_PreservesObsoleteUserWorkflow(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	custom := newTestDef("obsolete-custom-workflow-sync-test")
	custom.Builtin = false
	if err := store.Save(custom); err != nil {
		t.Fatalf("Save custom: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if _, err := store.Get(custom.ID); err != nil {
		t.Fatalf("custom workflow was pruned: %v", err)
	}
}

func TestSyncBuiltins_PruneFailureDoesNotBlockRefresh(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	bad := []byte(`id: ../obsolete-builtin-sync-test
name: Bad Builtin
trigger:
  on: task.created
steps:
  - id: s
    type: set_status
    config:
      status: todo
builtin: true
`)
	if err := os.WriteFile(filepath.Join(store.Dir(), "bad.yaml"), bad, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err = SyncBuiltins(store)
	if err == nil {
		t.Fatal("SyncBuiltins succeeded, want prune error")
	}
	defs, defsErr := BuiltinDefinitions()
	if defsErr != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", defsErr, len(defs))
	}
	if _, getErr := store.Get(defs[0].ID); getErr != nil {
		t.Fatalf("current builtin was not refreshed after prune failure: %v", getErr)
	}
}

// TestSyncBuiltins_IdempotentOnClean verifies the no-op path: calling
// SyncBuiltins twice in a row on a freshly seeded store must not bounce
// the UpdatedAt timestamp on every startup.
func TestSyncBuiltins_IdempotentOnClean(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("first SyncBuiltins: %v", err)
	}
	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	before, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("second SyncBuiltins: %v", err)
	}
	after, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("idempotent sync rewrote file: UpdatedAt before=%v after=%v",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// TestBuiltinTestingTask_RunTestOutputSchema verifies that the testing-task
// builtin's run_test step carries a non-empty OutputSchema that round-trips
// through YAML without alteration. Exact-string assertion so YAML folding
// or escaping errors surface immediately rather than silently passing an
// empty schema to codex.
func TestBuiltinTestingTask_RunTestOutputSchema(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	step := testingDef.StepByID("run_test")
	if step == nil {
		t.Fatal("run_test step not found in testing-task")
		return
	}
	const wantSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["PASS","FAIL"]},"outcome":{"type":"string","enum":["pass","product_bug","infra_failure","ambiguous_requirement","missing_evidence"]},"failures_markdown":{"type":"string"},"surface_kind":{"type":"string","enum":["web","cli","server","desktop","k8s","library","docs","none"]},"app_started":{"type":"boolean"},"start_command":{"type":"string"},"readiness_probe":{"type":"object","properties":{"command":{"type":"string"},"expected":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"},"url":{"type":"string"}},"required":["command","expected","actual","observed","output","status","url"],"additionalProperties":false},"manual_probes":{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"expected":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"}},"required":["command","expected","actual","observed","output","status"],"additionalProperties":false}},"automated_checks":{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"}},"required":["command","actual","observed","output","status"],"additionalProperties":false}},"unable_to_run_reason":{"type":"string"}},"required":["verdict","outcome","failures_markdown","surface_kind","app_started","start_command","readiness_probe","manual_probes","automated_checks","unable_to_run_reason"],"additionalProperties":false}`
	if step.Config.OutputSchema != wantSchema {
		t.Errorf("run_test.Config.OutputSchema =\n%q\nwant:\n%q", step.Config.OutputSchema, wantSchema)
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(step.Config.OutputSchema), &schema); err != nil {
		t.Fatalf("unmarshal output schema: %v", err)
	}
	required := map[string]bool{}
	for _, field := range schema.Required {
		required[field] = true
	}
	for field := range schema.Properties {
		if !required[field] {
			t.Fatalf("codex strict output schema requires every property; %q missing from required", field)
		}
	}
	requireCodexStrictOutputSchema(t, []byte(step.Config.OutputSchema))
}

func requireCodexStrictOutputSchema(t *testing.T, raw []byte) {
	t.Helper()

	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal output schema: %v", err)
	}
	requireCodexStrictSchemaNode(t, schema, "$")
}

func requireCodexStrictSchemaNode(t *testing.T, node any, path string) {
	t.Helper()

	switch v := node.(type) {
	case map[string]any:
		_, hasAdditionalProperties := v["additionalProperties"]
		if isCodexObjectSchema(v) && !hasAdditionalProperties {
			t.Fatalf("%s is object-shaped and must set additionalProperties:false", path)
		}
		propsValue, hasProps := v["properties"]
		if _, hasRequired := v["required"]; hasRequired && !hasProps {
			t.Fatalf("%s uses required without properties; Codex strict rejects requirement-only schemas", path)
		}
		if hasProps {
			props, ok := propsValue.(map[string]any)
			if !ok {
				t.Fatalf("%s.properties is %T, want object", path, propsValue)
			}
			additionalProperties, ok := v["additionalProperties"].(bool)
			if !ok || additionalProperties {
				t.Fatalf("%s must set additionalProperties:false", path)
			}
			requiredValue, ok := v["required"].([]any)
			if !ok {
				t.Fatalf("%s must require every property", path)
			}
			required := make(map[string]bool, len(requiredValue))
			for _, field := range requiredValue {
				name, ok := field.(string)
				if !ok {
					t.Fatalf("%s required field is %T, want string", path, field)
				}
				required[name] = true
			}
			for field := range props {
				if !required[field] {
					t.Fatalf("%s property %q missing from required", path, field)
				}
			}
		}
		for key, child := range v {
			requireCodexStrictSchemaNode(t, child, path+"."+key)
		}
	case []any:
		for _, child := range v {
			requireCodexStrictSchemaNode(t, child, path+"[]")
		}
	}
}

func isCodexObjectSchema(schema map[string]any) bool {
	if schemaType, ok := schema["type"].(string); ok && schemaType == "object" {
		return true
	}
	_, hasProperties := schema["properties"]
	_, hasRequired := schema["required"]
	_, hasAdditionalProperties := schema["additionalProperties"]
	return hasProperties || hasRequired || hasAdditionalProperties
}

// TestBuiltinTestingTask_NotestStillRunsTester asserts the real invariant:
// notest only downgrades evidence requirements (app-start exemption), it
// must never bypass the test-runner. skip_testing exists for explicit skip
// tags, but notest-tagged tasks must still route to run_test.
func TestBuiltinTestingTask_NotestStillRunsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
		return
	}
	for _, n := range maybe.Next {
		if n.When != nil && n.When.Value == "notest" {
			t.Fatalf("maybe_test must not branch on notest, got branch to %q", n.GoTo)
		}
	}
	if len(maybe.Next) == 0 {
		t.Fatal("maybe_test has no transitions")
	}
	if got := maybe.Next[len(maybe.Next)-1].GoTo; got != "run_test" {
		t.Fatalf("maybe_test fallthrough = %q, want run_test", got)
	}
}

func TestBuiltinTestingTask_TrivialSkipsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
		return
	}
	var gotTrivialBranch bool
	for _, n := range maybe.Next {
		if n.When != nil && n.When.Field == "task.tags" && n.When.Operator == "contains" && n.When.Value == "trivial" {
			gotTrivialBranch = true
			if n.GoTo != "skip_testing" {
				t.Fatalf("trivial branch goto = %q, want skip_testing", n.GoTo)
			}
		}
	}
	if !gotTrivialBranch {
		t.Fatal("maybe_test has no branch for task.tags contains trivial")
	}

	skip := testingDef.StepByID("skip_testing")
	if skip == nil {
		t.Fatal("skip_testing step not found in testing-task")
		return
	}
	if skip.Type != "set_status" || skip.Config.Status != "ready-pr" {
		t.Fatalf("skip_testing = %+v, want set_status to ready-pr", skip)
	}
}

func TestBuiltinTestingTask_SkipTestingSkipsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
		return
	}
	var gotBranch bool
	for _, n := range maybe.Next {
		if n.When != nil && n.When.Field == "task.tags" && n.When.Operator == "contains" && n.When.Value == "skip-testing" {
			gotBranch = true
			if n.GoTo != "skip_testing" {
				t.Fatalf("skip-testing branch goto = %q, want skip_testing", n.GoTo)
			}
		}
	}
	if !gotBranch {
		t.Fatal("maybe_test has no branch for task.tags contains skip-testing")
	}
}

func TestBuiltinTestingTask_SmallNonTestChoreSkipsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
		return
	}
	if got, err := ResolveTransition(maybe.Next, map[string]string{"task.tags": "backend,chore,small"}); err != nil || got != "maybe_skip_chore_size" {
		t.Fatalf("maybe_test transition = %q, %v; want maybe_skip_chore_size, nil", got, err)
	}

	size := testingDef.StepByID("maybe_skip_chore_size")
	if size == nil {
		t.Fatal("maybe_skip_chore_size step not found in testing-task")
		return
	}
	if got, err := ResolveTransition(size.Next, map[string]string{"task.tags": "backend,chore,small"}); err != nil || got != "maybe_skip_chore_test_tag" {
		t.Fatalf("maybe_skip_chore_size transition = %q, %v; want maybe_skip_chore_test_tag, nil", got, err)
	}

	risk := testingDef.StepByID("maybe_skip_chore_test_tag")
	if risk == nil {
		t.Fatal("maybe_skip_chore_test_tag step not found in testing-task")
		return
	}
	if got, err := ResolveTransition(risk.Next, map[string]string{"task.tags": "backend,chore,small"}); err != nil || got != "skip_testing" {
		t.Fatalf("maybe_skip_chore_test_tag transition = %q, %v; want skip_testing, nil", got, err)
	}
}

func TestBuiltinTestingTask_MediumChoreStillRunsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	size := testingDef.StepByID("maybe_skip_chore_size")
	if size == nil {
		t.Fatal("maybe_skip_chore_size step not found in testing-task")
		return
	}
	if got, err := ResolveTransition(size.Next, map[string]string{"task.tags": "backend,chore,medium"}); err != nil || got != "run_test" {
		t.Fatalf("maybe_skip_chore_size transition = %q, %v; want run_test, nil", got, err)
	}
}

func TestBuiltinTestingTask_TestChoreStillRunsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	risk := testingDef.StepByID("maybe_skip_chore_test_tag")
	if risk == nil {
		t.Fatal("maybe_skip_chore_test_tag step not found in testing-task")
		return
	}
	if got, err := ResolveTransition(risk.Next, map[string]string{"task.tags": "backend,test,chore,small"}); err != nil || got != "run_test" {
		t.Fatalf("maybe_skip_chore_test_tag transition = %q, %v; want run_test, nil", got, err)
	}
}

func mustBuiltinDefinition(t *testing.T, id string) *Definition {
	t.Helper()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for i := range defs {
		if defs[i].ID == id {
			return &defs[i]
		}
	}
	t.Fatalf("%s builtin definition not found", id)
	return nil
}

// TestBuiltinBestOfN_OptInTriggerPriority locks the opt-in contract: an
// untagged in-progress task must still select simple-task-implement, and only
// a task carrying the best-of-n tag selects simple-task-best-of-n-implement
// — which requires a strictly higher trigger priority so it wins the tie on
// the same task.status_changed/in-progress event.
func TestBuiltinBestOfN_OptInTriggerPriority(t *testing.T) {
	t.Parallel()

	implement := mustBuiltinDefinition(t, "simple-task-implement")
	bestOfN := mustBuiltinDefinition(t, "simple-task-best-of-n-implement")

	if bestOfN.Trigger.Priority <= implement.Trigger.Priority {
		t.Fatalf("simple-task-best-of-n-implement priority %d must be > simple-task-implement priority %d",
			bestOfN.Trigger.Priority, implement.Trigger.Priority)
	}

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	untagged := TaskInfo{ID: "t1", Status: "in-progress", Tags: []string{"backend"}}
	if got := engine.MatchWorkflow(untagged, "task.status_changed"); got == nil || got.ID != "simple-task-implement" {
		id := "<nil>"
		if got != nil {
			id = got.ID
		}
		t.Fatalf("untagged in-progress task matched %q, want simple-task-implement", id)
	}

	tagged := TaskInfo{ID: "t2", Status: "in-progress", Tags: []string{"backend", "best-of-n"}}
	if got := engine.MatchWorkflow(tagged, "task.status_changed"); got == nil || got.ID != "simple-task-best-of-n-implement" {
		id := "<nil>"
		if got != nil {
			id = got.ID
		}
		t.Fatalf("best-of-n-tagged in-progress task matched %q, want simple-task-best-of-n-implement", id)
	}
}

func TestSimpleTaskReview_DoesNotMatchLinkedPRTask(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	prePR := TaskInfo{ID: "pre-pr", Status: "ready-review"}
	if got := engine.MatchWorkflow(prePR, "task.status_changed"); got == nil || got.ID != "simple-task-review" {
		id := "<nil>"
		if got != nil {
			id = got.ID
		}
		t.Fatalf("pre-PR ready-review task matched %q, want simple-task-review", id)
	}

	linkedPR := TaskInfo{ID: "linked-pr", Status: "ready-review", PRNumber: 1981}
	if got := engine.MatchWorkflow(linkedPR, "task.status_changed"); got != nil {
		t.Fatalf("linked-PR ready-review task matched %q, want no pre-PR review workflow", got.ID)
	}
}

func TestSimpleTaskPR_SkipsReviewOnlyRoles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	codeAuthor := TaskInfo{ID: "code-author", Status: "ready-pr"}
	if got := engine.MatchWorkflow(codeAuthor, "task.status_changed"); got == nil || got.ID != "simple-task-pr" {
		id := "<nil>"
		if got != nil {
			id = got.ID
		}
		t.Fatalf("code-author ready-pr task matched %q, want simple-task-pr", id)
	}

	for _, role := range []string{"review", "test-runner", "human-review"} {
		t.Run(role, func(t *testing.T) {
			if got := engine.MatchWorkflow(TaskInfo{ID: role, Status: "ready-pr", Role: role}, "task.status_changed"); got != nil {
				t.Fatalf("%s ready-pr task matched %q, want no PR workflow", role, got.ID)
			}
		})
	}
}

// TestBuiltinBestOfN_DeclaresMechanicalSteps confirms the opt-in workflow
// wires both new step types with the promote step correctly cross-referencing
// the judge and best_of_n steps — a minimal regression net for the YAML
// staying in sync with model.go's step-type constants.
func TestBuiltinBestOfN_DeclaresMechanicalSteps(t *testing.T) {
	t.Parallel()

	def := mustBuiltinDefinition(t, "simple-task-best-of-n-implement")

	var sawBestOfN, sawPromote bool
	for i := range def.Steps {
		s := &def.Steps[i]
		switch s.Type {
		case StepBestOfN:
			sawBestOfN = true
			if s.Config.Attempts < 2 {
				t.Errorf("best_of_n step %q attempts = %d, want >= 2", s.ID, s.Config.Attempts)
			}
		case StepPromoteBestOfN:
			sawPromote = true
			if s.Config.JudgeStep == "" || s.Config.BestOfNStep == "" {
				t.Errorf("promote_best_of_n step %q missing judge_step/best_of_n_step", s.ID)
			}
		default:
			// Every other step type in this workflow (run_agent, verify_commits,
			// detect_tampering, verify_checks, set_status, ...) is out of scope
			// for this regression net.
		}
	}
	if !sawBestOfN {
		t.Error("simple-task-best-of-n-implement has no best_of_n step")
	}
	if !sawPromote {
		t.Error("simple-task-best-of-n-implement has no promote_best_of_n step")
	}
}

// TestBuiltinDefinitions_NeverHardcodeCommitSignFlags is the repo-wide
// acceptance probe for the commit-signing invariant: no builtin workflow
// prompt may hardcode -S. A keyless host cannot satisfy it, and `git commit
// -S` overrides the clone's commit.gpgsign=false, so a hardcoded flag parks
// the task with "gpg failed to sign the data". The per-step guard in
// TestBuiltinPRFix_TestFixPromptCarriesFailingTests only covered pr-fix's
// test_fix step, which is how simple-task-review's hardcoded -S survived.
func TestBuiltinDefinitions_NeverHardcodeCommitSignFlags(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for _, def := range defs {
		for _, step := range def.Steps {
			prompt := step.Config.Prompt
			if prompt == "" {
				continue
			}
			for line := range strings.Lines(prompt) {
				// "commit", not "git commit": prompts also spell this as a
				// bare "# commit -s to complete the merge".
				if !strings.Contains(line, "commit") {
					continue
				}
				if strings.Contains(line, "-S") {
					t.Fatalf("builtin %q step %q hardcodes commit sign flags; template {{commitsignflags .Vars}} instead:\n%s",
						def.ID, step.ID, line)
				}
			}
		}
	}
}

// An unseeded commit_sign_flags must render as -s, not as an empty string
// that produces a broken `git commit `. Only the pr-fix dispatcher seeds the
// variable, so every other workflow relies on this fallback.
func TestCommitSignFlagsVar_DefaultsToSignoffOnly(t *testing.T) {
	t.Parallel()

	if got := commitSignFlagsVar(nil); got != "-s" {
		t.Errorf("commitSignFlagsVar(nil) = %q, want -s", got)
	}
	if got := commitSignFlagsVar(map[string]string{WorkflowVarCommitSignFlags: "  "}); got != "-s" {
		t.Errorf("commitSignFlagsVar(blank) = %q, want -s", got)
	}
	if got := commitSignFlagsVar(map[string]string{WorkflowVarCommitSignFlags: "-s -S"}); got != "-s -S" {
		t.Errorf("commitSignFlagsVar(seeded) = %q, want -s -S", got)
	}
}
