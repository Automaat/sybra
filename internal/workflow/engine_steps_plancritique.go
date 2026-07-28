package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// execFlagPlanCritique appends a prominent note to the task body when the
// plan-critic's own verdict is REFINE or REJECT. It never blocks progression
// itself — review_plan's wait_human step already requires an explicit human
// action every time regardless of verdict — this only makes sure "the critic
// asked for changes" is visible right where that decision gets made, instead
// of requiring the human to open and read the full critique sidecar. An
// empty or unrecognized verdict (including a critic that didn't follow the
// skill's output contract) is treated the same as APPROVE: no note, no
// change in behavior from before this step existed.
func (e *Engine) execFlagPlanCritique(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	verdict := parsePlanCritiqueVerdict(t.PlanCritique)
	if verdict != "REFINE" && verdict != "REJECT" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "verdict: " + verdict}, nil
	}
	note := fmt.Sprintf(
		"## ⚠️ Plan Critic Verdict: %s\n\nThe plan critic did not approve this plan as written — review the findings in the critique sidecar before approving.",
		verdict,
	)
	if err := e.tasks.AppendTaskBody(taskID, note); err != nil {
		e.logger.Error("workflow.flag-plan-critique.append", "task_id", taskID, "err", err)
		return StepOutput{}, err
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: "verdict: " + verdict + " — flagged"}, nil
}

// verdictWordRe matches a whole word inflected from APPROVE/REFINE/REJECT
// (APPROVED, REJECTS, REFINING, ...), so prose like "rejected previously" or
// "refine the error message" (a finding's own wording, not a verdict) can't
// false-match, while a critic phrasing the verdict in a full sentence
// ("this plan is rejected") still resolves to the base verdict.
var verdictWordRe = regexp.MustCompile(`(?i)\b(APPROV(?:E|ES|ED|ING)|REFIN(?:E|ES|ED|ING)|REJECT(?:S|ED|ING)?)\b`)

// verdictColonLineRe matches the /plan-critic skill's current output
// contract (internal/skills/data/plan-critic.md, Phase 6): a bare
// "# Plan Review" title followed by "## Verdict: <VERDICT>" — the verdict
// sits on the Verdict heading's own line, not in prose below it. Tolerates
// 1-6 leading `#`s and captures the rest of that line for verdictWordRe.
var verdictColonLineRe = regexp.MustCompile(`(?im)^#{1,6}\s*Verdict:\s*(.*)$`)

// planReviewTitleRe matches the skill's older title-line contract,
// "# Plan Review: <VERDICT>", kept as a fallback for critiques produced
// before the Phase 6 format above, or a critic that reverts to it.
var planReviewTitleRe = regexp.MustCompile(`(?im)^#{1,6}\s*Plan Review:?\s*([A-Z]*)`)

// verdictSectionRe isolates the "## Verdict" section's prose body (up to the
// next heading or end of content) as a last-resort fallback for a critic
// that drifted from both contract shapes above and only states the verdict
// in a full sentence beneath the heading.
var verdictSectionRe = regexp.MustCompile(`(?is)#{1,6}\s*Verdict\s*\n(.*?)(\n#{1,6}\s|\z)`)

// canonicalVerdict maps a matched inflected verdict word (APPROVED,
// REJECTS, REFINING, ...) back to its base APPROVE/REFINE/REJECT form.
func canonicalVerdict(word string) string {
	upper := strings.ToUpper(word)
	switch {
	case strings.HasPrefix(upper, "APPROV"):
		return "APPROVE"
	case strings.HasPrefix(upper, "REFIN"):
		return "REFINE"
	case strings.HasPrefix(upper, "REJECT"):
		return "REJECT"
	default:
		return ""
	}
}

// PlanCritiqueVerdict extracts the verdict word from a plan-critique
// sidecar, checking three locations in order of how closely they match the
// skill's actual current contract: the "## Verdict: <VERDICT>" line
// (current), the older "# Plan Review: <VERDICT>" title line, and finally
// the "## Verdict" section's prose body for a critic that drifted from both.
// Deliberately does not scan the rest of the document (findings, required
// changes, etc.), where REFINE/REJECT can appear in unrelated prose. Returns
// "" — treated identically to APPROVE by the caller — when none of the three
// yield a recognizable verdict, preserving today's "any non-empty sidecar is
// fine" behavior for critics that don't follow the contract at all.
func PlanCritiqueVerdict(content string) string {
	for _, re := range []*regexp.Regexp{verdictColonLineRe, planReviewTitleRe, verdictSectionRe} {
		m := re.FindStringSubmatch(content)
		if m == nil {
			continue
		}
		if v := canonicalVerdict(verdictWordRe.FindString(m[1])); v != "" {
			return v
		}
	}
	return ""
}

func parsePlanCritiqueVerdict(content string) string {
	return PlanCritiqueVerdict(content)
}
