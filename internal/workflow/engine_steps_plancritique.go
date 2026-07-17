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

// verdictWordRe matches a whole-word APPROVE/REFINE/REJECT, so prose like
// "rejected previously" or "refine the error message" (a finding's own
// wording, not a verdict) can't false-match.
var verdictWordRe = regexp.MustCompile(`(?i)\b(APPROVE|REFINE|REJECT)\b`)

// planReviewTitleRe matches the /plan-critic skill's required title line,
// "# Plan Review: <VERDICT>", within the first few lines of the sidecar.
var planReviewTitleRe = regexp.MustCompile(`(?im)^#\s*Plan Review:?\s*([A-Z]*)`)

// verdictSectionRe isolates the skill's "## Verdict" section (its prose
// body, up to the next heading or end of content) as a fallback location.
var verdictSectionRe = regexp.MustCompile(`(?is)##\s*Verdict\s*\n(.*?)(\n##|\z)`)

// parsePlanCritiqueVerdict extracts the verdict word from a plan-critique
// sidecar. The /plan-critic skill's output contract states the verdict twice
// — the "# Plan Review: <VERDICT>" title line, and again in prose inside the
// "## Verdict" section — so this checks the title line first (the more
// precise, contract-exact location) and falls back to the Verdict section
// when a critic drifts from the exact title format (e.g. a bare "# Plan
// Review" heading with the actual verdict stated only in the section below).
// Deliberately does not scan the rest of the document (findings, required
// changes, etc.), where REFINE/REJECT can appear in unrelated prose. Returns
// "" — treated identically to APPROVE by the caller — when neither location
// yields a recognizable verdict, preserving today's "any non-empty sidecar
// is fine" behavior for critics that don't follow the contract at all.
func parsePlanCritiqueVerdict(content string) string {
	if m := planReviewTitleRe.FindStringSubmatch(content); m != nil {
		if v := verdictWordRe.FindString(m[1]); v != "" {
			return strings.ToUpper(v)
		}
	}
	if m := verdictSectionRe.FindStringSubmatch(content); m != nil {
		if v := verdictWordRe.FindString(m[1]); v != "" {
			return strings.ToUpper(v)
		}
	}
	return ""
}
