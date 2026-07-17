package workflow

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/skillinvoke"
	"github.com/Automaat/sybra/internal/skills"
)

// flatten collapses whitespace and case so a prose assertion survives
// incidental rewrapping of a YAML folded block scalar.
func flatten(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// TestBuiltinTestingTask_RunTestPromptDoesNotDuplicateSkillProcedure keeps
// run_test's prompt from re-growing a second copy of /sybra-test's procedure.
//
// The prompt used to restate ~130 lines of sybra-test.md inline (PROVE
// framing, angles of attack, SYBRA_HOME/sandbox handling, evidence-grounding
// rules, the full output contract) on top of telling the agent to invoke
// /sybra-test. The two copies had already silently drifted — the inline copy
// was missing sybra-test.md's flock-enforcement detail — which is exactly
// the failure mode this test exists to catch: a future edit to either the
// skill or this prompt that reintroduces the duplicated procedure text.
// /sybra-test is the sole canonical source; this prompt carries only the
// task-specific dynamic contract the skill cannot know statically, and the
// skill-receipt mechanism (engine_skill_receipt.go) guarantees the skill
// actually runs.
func TestBuiltinTestingTask_RunTestPromptDoesNotDuplicateSkillProcedure(t *testing.T) {
	t.Parallel()

	def := mustBuiltinDefinition(t, "testing-task")
	step := def.StepByID("run_test")
	if step == nil {
		t.Fatal("run_test step not found in testing-task")
		return
	}
	prompt := step.Config.Prompt
	if prompt == "" {
		t.Fatal("run_test prompt is empty")
	}
	flat := flatten(prompt)

	// Procedural vocabulary that belongs solely to sybra-test.md's Procedure
	// and Rules sections. Reappearance here means the procedure has been
	// duplicated back into the workflow prompt.
	for _, banned := range []string{
		"prove the implementation does not satisfy",
		"attack the edges",
		"manual testing is mandatory by default",
		"set surface_kind to exactly one canonical token",
		"you decide how to start and drive the real app",
		"for each claimed defect you must have",
		"prefer a single json object as your final response",
		"flock",
	} {
		if strings.Contains(flat, banned) {
			t.Errorf("run_test prompt duplicates /sybra-test procedure text (%q) — that procedure must live only in internal/skills/data/sybra-test.md", banned)
		}
	}

	// Exactly one skill must be invoked — this is what makes
	// engine_skill_receipt.go's conformance enforcement engage
	// (app_workflow.go's workflowRequestedSkill requires len==1).
	names := skillinvoke.InvokedNames(prompt)
	if len(names) != 1 || names[0] != "sybra-test" {
		t.Fatalf("run_test prompt must invoke exactly the sybra-test skill, got %v", names)
	}

	// The task-specific dynamic contract sybra-test.md cannot know statically
	// must still be present — trimming the duplicated procedure must not
	// have swept up genuinely unique content.
	for _, want := range []string{
		`{{if getvar .Vars "testing_reask_note"}}`,
		"{{if .Task.ManualTest.Kind}}",
		"{{if acceptanceledger .Task.Body}}",
		"TEST_VERDICT: PASS",
		"TEST_VERDICT: FAIL",
		"output_schema",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("run_test prompt is missing required dynamic contract marker %q", want)
		}
	}
}

// TestBuiltinTestingTask_SkillOwnsTestProcedure is the mirror check: the
// canonical procedure text must actually still exist somewhere. Without this,
// a future edit could strip the procedure from both the workflow prompt and
// the skill, leaving zero sources instead of one.
func TestBuiltinTestingTask_SkillOwnsTestProcedure(t *testing.T) {
	t.Parallel()

	data, err := skills.FS.ReadFile("data/sybra-test.md")
	if err != nil {
		t.Fatalf("read sybra-test skill: %v", err)
	}
	flat := flatten(string(data))
	for _, want := range []string{
		"manual testing is mandatory by default",
		"test_verdict: pass",
		"test_verdict: fail",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("internal/skills/data/sybra-test.md is missing canonical procedure text %q", want)
		}
	}
}
