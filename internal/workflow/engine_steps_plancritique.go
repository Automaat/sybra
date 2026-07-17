package workflow

import (
	"fmt"
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

// parsePlanCritiqueVerdict extracts the verdict word from a plan-critique
// sidecar. The /plan-critic skill's output contract fixes the verdict to a
// "# Plan Review: <APPROVE|REFINE|REJECT>" heading, but scans only the first
// few lines for a "PLAN REVIEW:" marker rather than requiring it as literally
// line one (tolerating a leading blank line or code fence) — and stops well
// short of scanning the whole document, since the word REFINE/REJECT can
// legitimately appear in later findings prose without being the verdict.
func parsePlanCritiqueVerdict(content string) string {
	const marker = "PLAN REVIEW:"
	lines := strings.SplitN(content, "\n", 6)
	for _, line := range lines {
		upper := strings.ToUpper(line)
		_, rest, found := strings.Cut(upper, marker)
		if !found {
			continue
		}
		for _, v := range []string{"APPROVE", "REFINE", "REJECT"} {
			if strings.Contains(rest, v) {
				return v
			}
		}
	}
	return ""
}
