//go:build e2e

package sybra

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// setupCrossProviderEnv creates an e2e env with both fake-claude and fake-codex
// on PATH, separate scenario files for each provider, and the test-review-fix
// workflow loaded.
func setupCrossProviderEnv(t *testing.T, defaultProvider string, claudeScenarios, codexScenarios []string) *e2eEnv {
	t.Helper()

	claudeSF := filepath.Join(t.TempDir(), "claude-scenarios.txt")
	if err := os.WriteFile(claudeSF, []byte(strings.Join(claudeScenarios, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	codexSF := filepath.Join(t.TempDir(), "codex-scenarios.txt")
	if err := os.WriteFile(codexSF, []byte(strings.Join(codexScenarios, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	env := setupE2EProvider(t, defaultProvider, "")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", claudeSF)
	t.Setenv("FAKE_CODEX_SCENARIO_FILE", codexSF)
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")
	t.Setenv("FAKE_CODEX_SCENARIO", "")

	// Load test-review-fix workflow.
	src, err := os.ReadFile("../../internal/workflow/testdata/test-review-fix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(env.wfStore.Dir(), "test-review-fix.yaml")
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatal(err)
	}

	return env
}

// TestE2E_CrossProvider_ReviewThenFix drives the test-review-fix workflow
// through implement → maybe_review → code_review (cross-provider) → fix_review
// and asserts all steps execute with correct roles and providers.
func TestE2E_CrossProvider_ReviewThenFix(t *testing.T) {
	// Default provider is claude → implement runs on claude.
	// code_review has provider: cross → runs on codex.
	// fix_review has no provider → runs on claude (default).
	env := setupCrossProviderEnv(t, "claude",
		[]string{"success", "success"}, // claude: implement, fix_review
		[]string{"success"},            // codex: code_review (cross)
	)

	created, err := env.tasks.Create("cross-provider review test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			(tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	// Verify all expected steps ran in order.
	stepIDs := stepIDsFromHistory(tk.Workflow)
	for _, want := range []string{"implement", "maybe_review", "code_review", "fix_review"} {
		if !slices.Contains(stepIDs, want) {
			t.Errorf("step %q missing from history: %v", want, stepIDs)
		}
	}

	// Verify step ordering: implement < code_review < fix_review.
	implIdx := slices.Index(stepIDs, "implement")
	reviewIdx := slices.Index(stepIDs, "code_review")
	fixIdx := slices.Index(stepIDs, "fix_review")
	if implIdx >= reviewIdx || reviewIdx >= fixIdx {
		t.Errorf("step order wrong: want implement(%d) < code_review(%d) < fix_review(%d)",
			implIdx, reviewIdx, fixIdx)
	}

	// Verify agent roles were recorded.
	roles := agentRunRoles(tk)
	for _, want := range []string{"implementation", "review", "fix-review"} {
		if !slices.Contains(roles, want) {
			t.Errorf("role %q missing from agent runs: %v", want, roles)
		}
	}
}

// TestE2E_CrossProvider_ReviewSidecarPersisted drives the cross-provider
// review flow but uses the code_review_success scenario so the review
// agent calls `sybra-cli update --code-review`. Asserts the CodeReview
// sidecar is populated on the task once the workflow completes — proving
// the end-to-end save path works: scenario → sybra-cli flag → Update →
// CodeReviewStore → Store.Get → Task.CodeReview → frontend JSON.
func TestE2E_CrossProvider_ReviewSidecarPersisted(t *testing.T) {
	env := setupCrossProviderEnv(t, "claude",
		[]string{"success", "success"},  // claude: implement, fix_review
		[]string{"code_review_success"}, // codex: code_review writes sidecar via CLI
	)

	created, err := env.tasks.Create("review sidecar persisted", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			(tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	if tk.CodeReview == "" {
		t.Fatalf("CodeReview sidecar empty after workflow completed — expected scenario to persist it")
	}
	if !strings.Contains(tk.CodeReview, "Code Review") {
		t.Errorf("CodeReview content unexpected: %q", tk.CodeReview)
	}

	// Sidecar file must exist on disk under the configured tasks dir.
	sidecar := filepath.Join(env.taskDir, "tasks", created.ID+".review.md")
	if _, statErr := os.Stat(sidecar); os.IsNotExist(statErr) {
		t.Errorf("expected sidecar at %s, file does not exist", sidecar)
	} else if statErr != nil {
		t.Errorf("stat sidecar %s: %v", sidecar, statErr)
	}
}

// TestE2E_CrossProvider_NoreviewTagSkipsReview verifies that a task tagged
// "noreview" bypasses code_review and fix_review via the maybe_review
// condition step.
func TestE2E_CrossProvider_NoreviewTagSkipsReview(t *testing.T) {
	env := setupCrossProviderEnv(t, "claude",
		[]string{"success"}, // claude: implement only
		[]string{},          // codex: nothing (review skipped)
	)

	created, err := env.tasks.Create("noreview test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	tags := []string{"noreview"}
	if _, err := env.tasks.Update(created.ID, task.Update{Tags: &tags}); err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			(tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	stepIDs := stepIDsFromHistory(tk.Workflow)

	// maybe_review condition must still execute.
	if !slices.Contains(stepIDs, "maybe_review") {
		t.Errorf("maybe_review missing — condition must still run: %v", stepIDs)
	}

	// Review and fix steps must NOT run.
	if slices.Contains(stepIDs, "code_review") {
		t.Errorf("code_review must be skipped with noreview tag: %v", stepIDs)
	}
	if slices.Contains(stepIDs, "fix_review") {
		t.Errorf("fix_review must be skipped with noreview tag: %v", stepIDs)
	}

	// No review/fix-review agent roles spawned.
	roles := agentRunRoles(tk)
	if slices.Contains(roles, "review") {
		t.Errorf("review agent must not spawn with noreview tag: %v", roles)
	}
	if slices.Contains(roles, "fix-review") {
		t.Errorf("fix-review agent must not spawn with noreview tag: %v", roles)
	}
}

// TestE2E_CrossProvider_ReviewUsesOppositeProvider verifies that the
// code_review step dispatches to the opposite provider via provider: cross.
func TestE2E_CrossProvider_ReviewUsesOppositeProvider(t *testing.T) {
	codexArgsLog := filepath.Join(t.TempDir(), "codex-args.log")
	claudeArgsLog := filepath.Join(t.TempDir(), "claude-args.log")

	env := setupCrossProviderEnv(t, "claude",
		[]string{"success", "success"}, // claude: implement, fix_review
		[]string{"success"},            // codex: code_review
	)
	t.Setenv("FAKE_CODEX_ARGS_LOG", codexArgsLog)
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", claudeArgsLog)

	created, err := env.tasks.Create("provider verify test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			(tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	// Codex fake must have been invoked (review step).
	if _, err := os.Stat(codexArgsLog); err != nil {
		t.Fatalf("codex args log not written — review step did not invoke codex: %v", err)
	}
	codexArgs, err := os.ReadFile(codexArgsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codexArgs), "Review") {
		t.Errorf("codex invocation should contain review prompt, got:\n%s", string(codexArgs))
	}
}

// TestE2E_CrossProvider_ReviewUsesOppositeProvider_DefaultCodex verifies the
// inverse routing: when default provider is codex, provider: cross dispatches
// the review step to claude.
func TestE2E_CrossProvider_ReviewUsesOppositeProvider_DefaultCodex(t *testing.T) {
	codexArgsLog := filepath.Join(t.TempDir(), "codex-args.log")
	claudeArgsLog := filepath.Join(t.TempDir(), "claude-args.log")

	env := setupCrossProviderEnv(t, "codex",
		[]string{"success"},            // claude: code_review (cross)
		[]string{"success", "success"}, // codex: implement, fix_review
	)
	t.Setenv("FAKE_CODEX_ARGS_LOG", codexArgsLog)
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", claudeArgsLog)

	created, err := env.tasks.Create("provider verify inverse test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			(tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	if _, err := os.Stat(claudeArgsLog); err != nil {
		t.Fatalf("claude args log not written — review step did not invoke claude: %v", err)
	}
	claudeArgs, err := os.ReadFile(claudeArgsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(claudeArgs), "Review") {
		t.Errorf("claude invocation should contain review prompt, got:\n%s", string(claudeArgs))
	}

	if _, err := os.Stat(codexArgsLog); err != nil {
		t.Fatalf("codex args log not written — default-provider steps did not invoke codex: %v", err)
	}
	codexArgs, err := os.ReadFile(codexArgsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codexArgs), "Fix review comments") {
		t.Errorf("codex invocation should contain fix-review prompt, got:\n%s", string(codexArgs))
	}
}

// TestE2E_CrossProvider_DualPlan exercises the dual-provider plan flow in
// builtin simple-task end-to-end: triage → parallel{plan_claude (claude
// opus), plan_codex (codex)} → converge_plans → require_plan → maybe_critique
// (skipped via nocritic) → review_plan. Asserts each parallel child ran on
// its declared provider, both plan-draft sidecars round-tripped through
// disk, and the converged plan landed in Task.Plan.
func TestE2E_CrossProvider_DualPlan(t *testing.T) {
	// Default provider claude → triage runs on claude. plan_claude is
	// pinned to claude, plan_codex to codex. Converge runs on the default
	// (claude) since the workflow doesn't specify a provider for it.
	env := setupCrossProviderEnv(t, "claude",
		[]string{
			"triage_to_planning_nocritic", // triage (claude)
			"write_sidecar_success",       // plan_claude → plan-draft sidecar
			"write_sidecar_success",       // converge_plans → final Plan sidecar
		},
		[]string{
			"write_sidecar_success", // plan_codex → plan-draft sidecar
		},
	)
	loadBuiltinWorkflow(t, env, "simple-task-plan")

	created, err := env.tasks.Create("dual-plan e2e", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "simple-task-plan"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow reaches review_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Step history: parent `plan` is recorded once (children don't get
	// their own StepRecord — the engine collapses them into the parent).
	stepIDs := stepIDsFromHistory(tk.Workflow)
	for _, want := range []string{"triage", "plan", "converge_plans", "require_plan", "maybe_critique"} {
		if !slices.Contains(stepIDs, want) {
			t.Errorf("step %q missing from history: %v", want, stepIDs)
		}
	}

	// Both plan-draft sidecars must be present and keyed by child step ID.
	if got := tk.PlanDrafts["plan_claude"]; got == "" {
		t.Errorf("PlanDrafts[plan_claude] empty — claude child did not write its draft")
	}
	if got := tk.PlanDrafts["plan_codex"]; got == "" {
		t.Errorf("PlanDrafts[plan_codex] empty — codex child did not write its draft")
	}

	// Convergence must have produced the canonical Plan sidecar.
	if tk.Plan == "" {
		t.Errorf("Task.Plan empty after converge_plans — synthesized plan not ingested")
	}

	// Each parallel child must have run on its declared provider. We look
	// at AgentRuns rather than the parent step record (which doesn't carry
	// per-child provider).
	gotProviders := map[string]string{} // role -> provider (for plan-role agents only)
	for _, ar := range tk.AgentRuns {
		if ar.Role != "plan" {
			continue
		}
		// Distinguish plan_claude vs plan_codex by provider — the role is
		// the same for all three plan-role agents (parallel children + converge).
		if ar.Provider != "" {
			gotProviders[ar.Provider] = ar.Provider
		}
	}
	for _, want := range []string{"claude", "codex"} {
		if _, ok := gotProviders[want]; !ok {
			t.Errorf("expected at least one plan-role agent on provider %q; agent runs: %+v", want, tk.AgentRuns)
		}
	}

	// Sidecar files must exist on disk under tasks dir.
	for _, name := range []string{"plan_claude", "plan_codex"} {
		path := filepath.Join(env.taskDir, "tasks", created.ID+".plan-draft-"+name+".md")
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected plan-draft sidecar at %s, file does not exist", path)
		}
	}
	planPath := filepath.Join(env.taskDir, "tasks", created.ID+".plan.md")
	if _, statErr := os.Stat(planPath); os.IsNotExist(statErr) {
		t.Errorf("expected synthesized plan sidecar at %s, file does not exist", planPath)
	}
}

// TestE2E_DualPlan_ChildRetryThenSucceed verifies the per-child retry path
// in the parallel block: plan_codex's first invocation fails, the engine
// re-spawns just that child within max_retries, the retry succeeds, and the
// parent step advances normally to converge_plans.
func TestE2E_DualPlan_ChildRetryThenSucceed(t *testing.T) {
	// Codex scenarios consumed in order: fail_exit (first call) →
	// write_sidecar_success (retry). Builtin simple-task gives plan_codex
	// max_retries: 1, so exactly one retry is permitted.
	env := setupCrossProviderEnv(t, "claude",
		[]string{
			"triage_to_planning_nocritic", // triage
			"write_sidecar_success",       // plan_claude
			"write_sidecar_success",       // converge_plans
		},
		[]string{
			"fail_exit",             // plan_codex first attempt — exits non-zero
			"write_sidecar_success", // plan_codex retry — writes the draft
		},
	)
	loadBuiltinWorkflow(t, env, "simple-task-plan")

	created, err := env.tasks.Create("dual-plan retry e2e", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "simple-task-plan"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow reaches review_plan after retry", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Both drafts present despite the first codex failure.
	if got := tk.PlanDrafts["plan_codex"]; got == "" {
		t.Errorf("PlanDrafts[plan_codex] empty — retry did not write the draft")
	}
	if got := tk.PlanDrafts["plan_claude"]; got == "" {
		t.Errorf("PlanDrafts[plan_claude] empty — claude child did not write its draft")
	}
	if tk.Plan == "" {
		t.Errorf("Task.Plan empty after converge_plans")
	}

	// We expect at least 2 codex invocations recorded against this task —
	// the failed first attempt and the successful retry.
	codexRuns := 0
	for _, ar := range tk.AgentRuns {
		if ar.Provider == "codex" {
			codexRuns++
		}
	}
	if codexRuns < 2 {
		t.Errorf("expected ≥2 codex agent runs (fail + retry), got %d in: %+v", codexRuns, tk.AgentRuns)
	}

	// Parent step record must be `completed` — the retry succeeded so no
	// child ended in a failed terminal state.
	for _, rec := range tk.Workflow.StepHistory {
		if rec.StepID == "plan" && rec.Status != "completed" {
			t.Errorf("parent plan step status = %q, want completed (retry succeeded so no terminal failure)", rec.Status)
		}
	}
}

// TestE2E_DualPlan_ChildExhaustedFailsParent covers the failure mode where
// a parallel child burns through max_retries and the parent step is
// recorded as failed. Asserts: parent record status=failed, the failed
// child's draft is missing, the surviving child's draft is present.
func TestE2E_DualPlan_ChildExhaustedFailsParent(t *testing.T) {
	// plan_codex max_retries: 1 → 2 attempts total. Both fail.
	env := setupCrossProviderEnv(t, "claude",
		[]string{
			"triage_to_planning_nocritic", // triage
			"write_sidecar_success",       // plan_claude
			// converge_plans may run depending on workflow Next semantics;
			// give it a scenario in case (failed parent currently still
			// advances via the unconditional `goto: converge_plans`).
			"write_sidecar_success",
		},
		[]string{
			"fail_exit", // plan_codex first attempt
			"fail_exit", // plan_codex retry — also fails, exhausted
		},
	)
	loadBuiltinWorkflow(t, env, "simple-task-plan")

	created, err := env.tasks.Create("dual-plan exhausted e2e", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "simple-task-plan"); err != nil {
		t.Fatal(err)
	}

	// Wait until the parent step record exists in history (terminal for
	// the parallel block, regardless of aggregate status). The workflow
	// may keep moving past `plan`; we only assert what the history shows.
	waitFor(t, 30*time.Second, "parent plan step recorded with status=failed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil || tk.Workflow == nil {
			return false
		}
		for _, rec := range tk.Workflow.StepHistory {
			if rec.StepID == "plan" {
				return rec.Status == "failed"
			}
		}
		return false
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// plan_claude's draft should still be present — its sibling failing
	// must not affect the surviving child's sidecar.
	if got := tk.PlanDrafts["plan_claude"]; got == "" {
		t.Errorf("PlanDrafts[plan_claude] empty — failure of sibling should not block this child")
	}
	// plan_codex's draft must NOT be present — it failed twice.
	if got := tk.PlanDrafts["plan_codex"]; got != "" {
		t.Errorf("PlanDrafts[plan_codex] = %q, want empty (both attempts failed)", got)
	}

	// Two codex invocations — the original + one retry.
	codexRuns := 0
	for _, ar := range tk.AgentRuns {
		if ar.Provider == "codex" {
			codexRuns++
		}
	}
	if codexRuns != 2 {
		t.Errorf("expected exactly 2 codex agent runs (initial + 1 retry), got %d", codexRuns)
	}
}
