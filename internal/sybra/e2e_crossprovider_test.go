//go:build !short

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
