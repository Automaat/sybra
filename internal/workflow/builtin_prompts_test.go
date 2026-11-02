package workflow

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/prompts"
)

func renderBuiltinPrompt(t *testing.T, workflowID, stepID string) string {
	t.Helper()
	step := mustBuiltinDefinition(t, workflowID).StepByID(stepID)
	if step == nil {
		t.Fatalf("%s/%s not found", workflowID, stepID)
	}
	rendered, err := RenderTemplate(step.Config.Prompt, TemplateContext{
		Task: TaskInfo{ID: "task-1", ProjectID: "o/r", PRNumber: 7},
		Vars: map[string]string{WorkflowVarSidecarDir: "/sidecars"},
	})
	if err != nil {
		t.Fatalf("render %s/%s: %v", workflowID, stepID, err)
	}
	return rendered
}

func TestBuiltinAgentPromptsDeliverTheirInstructionsInline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		workflow, step, prompt string
		wants                  []string
	}{
		{"simple-task-review", "code_review_simple", prompts.AdversarialReview, []string{"base ref `origin/main`", "/sidecars/.sybra-review-task-1.md", `{"verdict":"FAILED"}`}},
		{"pr-review", "review_simple", prompts.AdversarialReview, []string{"https://github.com/o/r/pull/7", "--status human-required"}},
		{"testing-task", "run_test", prompts.AdversarialTest, []string{"Test task task-1", "/sidecars/.sybra-test-evidence-task-1", "TEST_VERDICT: PASS"}},
		{"simple-task-plan", "critique_plan", prompts.PlanCritic, []string{"task task-1", "/sidecars/.sybra-critique-task-1.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			t.Parallel()
			rendered := renderBuiltinPrompt(t, tc.workflow, tc.step)
			if !strings.Contains(rendered, strings.TrimSpace(prompts.MustGet(tc.prompt))) {
				t.Errorf("rendered prompt does not contain the %s text verbatim", tc.prompt)
			}
			for _, want := range tc.wants {
				if !strings.Contains(rendered, want) {
					t.Errorf("rendered prompt missing %q", want)
				}
			}
		})
	}
}

func TestBuiltinDefinitionsInvokeNoSkill(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	for i := range defs {
		for j := range defs[i].Steps {
			checkStepInvokesNoSkill(t, defs[i].ID, &defs[i].Steps[j])
		}
	}
}

func checkStepInvokesNoSkill(t *testing.T, workflowID string, step *Step) {
	t.Helper()
	for _, banned := range []string{"/adversarial-review", "/staff-code-review", "/sybra-test", "/plan-critic", "$adversarial"} {
		if strings.Contains(step.Config.Prompt, banned) {
			t.Errorf("%s/%s prompt invokes %s", workflowID, step.ID, banned)
		}
	}
	for _, tool := range step.Config.AllowedTools {
		if tool == "Skill" {
			t.Errorf("%s/%s still grants the Skill tool", workflowID, step.ID)
		}
	}
	for i := range step.Parallel {
		checkStepInvokesNoSkill(t, workflowID, &step.Parallel[i])
	}
}

// A task can be parked on a step a new build no longer runs. Each retired step
// must stay resolvable and lead to the review that replaced it, or the task fails
// on a missing step.
func TestBuiltinRetiredReviewStepsResumeTheReview(t *testing.T) {
	t.Parallel()

	cases := []struct {
		workflow, step, want string
	}{
		{"simple-task-review", "code_review_staff", "code_review_simple"},
		{"pr-review", "review_staff", "review_simple"},
		{"pr-review", "triage_review", "review_simple"},
		{"pr-review", "pick_review_method", "review_simple"},
	}
	for _, tc := range cases {
		t.Run(tc.workflow+"/"+tc.step, func(t *testing.T) {
			t.Parallel()
			step := mustBuiltinDefinition(t, tc.workflow).StepByID(tc.step)
			if step == nil {
				t.Fatal("retired step not found")
			}
			if step.Type != StepCondition || len(step.Next) != 1 || step.Next[0].GoTo != tc.want {
				t.Fatalf("step = %+v, want a condition going to %s", step, tc.want)
			}
		})
	}
}

func TestPickReviewMethodSkipsOnlyAZeroLineDiff(t *testing.T) {
	t.Parallel()

	step := mustBuiltinDefinition(t, "simple-task-review").StepByID("pick_review_method")
	if step == nil {
		t.Fatal("pick_review_method not found")
	}
	for verdict, want := range map[string]string{
		triageVerdictSkip:   "done_review",
		triageVerdictReview: "code_review_simple",
	} {
		got := ""
		for _, next := range step.Next {
			if next.When == nil || EvalCondition(*next.When, map[string]string{"vars.step.triage_review.output": verdict}) {
				got = next.GoTo
				break
			}
		}
		if got != want {
			t.Errorf("verdict %q routes to %q, want %q", verdict, got, want)
		}
	}
}
