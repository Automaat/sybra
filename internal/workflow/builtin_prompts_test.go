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

// A task can be parked on a step a new build no longer routes to. Each retired
// agent step must run the same review, because the resume sweep only restarts
// agent steps and a retry rewinds to the step that produced the verdict.
func TestBuiltinRetiredReviewStepsRunTheSameReview(t *testing.T) {
	t.Parallel()

	cases := []struct {
		workflow, retired, current string
	}{
		{"simple-task-review", "code_review_staff", "code_review_simple"},
		{"pr-review", "review_staff", "review_simple"},
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			t.Parallel()
			def := mustBuiltinDefinition(t, tc.workflow)
			retired, current := def.StepByID(tc.retired), def.StepByID(tc.current)
			if retired == nil || current == nil {
				t.Fatalf("steps missing: retired=%v current=%v", retired, current)
			}
			if !isResumableStepType(retired.Type) {
				t.Errorf("retired step type %q cannot resume", retired.Type)
			}
			if retired.Config.Prompt != current.Config.Prompt || retired.Config.OutputSchema != current.Config.OutputSchema {
				t.Error("retired step does not run the same review")
			}
			if strings.Contains(strings.ToLower(retired.Name), "staff") {
				t.Errorf("retired step name %q still describes staff review", retired.Name)
			}
		})
	}
}

// A snapshot pinned before skills were removed names a step and a prompt the
// provider can no longer run, so the execution must move onto the live builtin.
func TestResolveExecutionDefinitionLeavesARetiredSkillSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	live := *mustBuiltinDefinition(t, "simple-task-review")
	if err := store.Save(live); err != nil {
		t.Fatal(err)
	}
	stale := live
	stale.Description = "before skills were removed"
	stale.Steps = []Step{{ID: "code_review_staff", Type: StepRunAgent, Config: StepConfig{Role: "review", Prompt: "Run /staff-code-review"}}}
	hash, err := store.SaveSnapshot(stale)
	if err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	info := TaskInfo{ID: "t1", Workflow: &Execution{WorkflowID: live.ID, DefinitionHash: hash, CurrentStep: "code_review_staff"}}

	got, err := engine.resolveExecutionDefinition("t1", info)
	if err != nil {
		t.Fatal(err)
	}
	step := got.StepByID("code_review_staff")
	if step == nil || strings.Contains(step.Config.Prompt, "/staff-code-review") {
		t.Fatalf("resolved step = %+v, want the live inline prompt", step)
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
