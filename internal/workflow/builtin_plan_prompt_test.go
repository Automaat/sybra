package workflow

import (
	"strings"
	"testing"
)

// scrutinyBlock returns the plan prompt's step-3 scrutiny requirements, sliced
// out of the surrounding prompt.
//
// Asserting against the whole prompt is not good enough: "Verification" also
// appears as artifact C's `## Verification` template header and in the JSON
// contract spec, and "rejected" appears in artifact A's spec — so a
// whole-prompt Contains passes even with the scrutiny bullets deleted.
func scrutinyBlock(t *testing.T, prompt string) string {
	t.Helper()
	const start = "3. Before committing to an approach"
	const end = "4. Write these"
	i := strings.Index(prompt, start)
	if i < 0 {
		t.Fatalf("plan prompt has no step-3 scrutiny block (looked for %q)", start)
	}
	j := strings.Index(prompt[i:], end)
	if j < 0 {
		t.Fatalf("plan prompt has no step-4 artifact block after scrutiny (looked for %q)", end)
	}
	return prompt[i : i+j]
}

// TestBuiltinSimpleTaskPlan_PromptStatesScrutinyInline locks the plan prompt
// against re-delegating its scrutiny to a review skill.
//
// The prompt used to tell the agent to convene `/council core-eng`, with an
// "if /council is unavailable in this environment, plan directly" opt-out that
// nothing could verify. 69 of 79 traced plan runs took that opt-out silently
// while still writing council-shaped prose into the research artifact, and no
// Go code enforced, measured, or recorded whether the council ever convened —
// so the regression was invisible for weeks. Any future escape hatch of this
// shape must fail here instead.
func TestBuiltinSimpleTaskPlan_PromptStatesScrutinyInline(t *testing.T) {
	t.Parallel()

	plan := mustBuiltinDefinition(t, "simple-task-plan")
	step := plan.StepByID("plan")
	if step == nil {
		t.Fatal("plan step not found in simple-task-plan")
		return
	}
	prompt := step.Config.Prompt

	for _, banned := range []string{"council", "persona"} {
		if strings.Contains(strings.ToLower(prompt), banned) {
			t.Errorf("plan prompt references %q — scrutiny must be stated inline, not delegated to a skill", banned)
		}
	}
	// An "if X is unavailable, do Y instead" opt-out is unfalsifiable from
	// outside the agent: it reads as compliance whichever branch is taken.
	if strings.Contains(strings.ToLower(prompt), "unavailable") {
		t.Error("plan prompt offers an unverifiable availability escape hatch")
	}

	// Each lens the council supplied must survive as a labelled requirement in
	// the scrutiny block itself. The trailing colon is what distinguishes a
	// requirement from prose elsewhere in the prompt.
	block := scrutinyBlock(t, prompt)
	for _, required := range []string{
		"- Simplicity:",
		"- Reuse:",
		"- Deletability:",
		"- Failure modes:",
		"- Second-order effects:",
		"- Cost:",
		"- Worth:",
		"- Verification:",
	} {
		if !strings.Contains(block, required) {
			t.Errorf("scrutiny block is missing the %q requirement", required)
		}
	}
	if !strings.Contains(block, "alternative you rejected") {
		t.Error("scrutiny block does not require naming a rejected alternative")
	}
}

// TestBuiltinSimpleTaskPlan_PlanStepGrantsNoSkillOrAgent makes the council's
// removal structural on the claude path rather than purely advisory.
//
// The plan prompt names neither a skill nor a subagent, so granting either
// leaves the council one unprompted Skill call away (the plugin cache is
// discoverable), at 3.44x the cost of a plain plan run. agent.max_cost_usd
// cannot pre-empt that: providers report spend only on their terminal event,
// so the ceiling fires after the money is gone.
//
// Only provider_claude.go reads AllowedTools — codex and copilot ignore it —
// so this bounds the claude path and the empty prompt carries the rest.
func TestBuiltinSimpleTaskPlan_PlanStepGrantsNoSkillOrAgent(t *testing.T) {
	t.Parallel()

	plan := mustBuiltinDefinition(t, "simple-task-plan")
	step := plan.StepByID("plan")
	if step == nil {
		t.Fatal("plan step not found in simple-task-plan")
		return
	}
	if len(step.Config.AllowedTools) == 0 {
		t.Fatal("plan step has no allowed_tools — the permission precedence would fall through to a bypass mode")
	}
	for _, tool := range step.Config.AllowedTools {
		if tool == "Skill" || tool == "Agent" {
			t.Errorf("plan step grants %q, but its prompt invokes neither — that is the capability the council needs", tool)
		}
	}
}
