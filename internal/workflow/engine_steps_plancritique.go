package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	planCritiqueVerdictVar              = "plan_critique_verdict"
	planCritiqueVerdictSourceStepVar    = "plan_critique_verdict_source_step"
	planCritiqueVerdictApprove          = "APPROVE"
	planCritiqueVerdictRefine           = "REFINE"
	planCritiqueVerdictReject           = "REJECT"
	planCritiqueVerdictAutoRetryCap     = 2
	planCritiqueVerdictAutoRetryBackoff = 2 * time.Minute
	planCritiqueReaskNoteVar            = "plan_critique_reask_note"
	planCritiqueSourceStep              = "critique_plan"
)

var planCritiqueMalformedReask = "Your previous final message could not be parsed as a valid verdict. " +
	"Keep the full markdown critique in the sidecar file, but your FINAL message of the turn " +
	"must be ONLY this JSON object (no prose, no markdown fences, no extra keys): " +
	`{"verdict":"APPROVE"}, {"verdict":"REFINE"}, or {"verdict":"REJECT"}.`

// execFlagPlanCritique appends a prominent note to the task body when the
// plan-critic's own verdict is REFINE or REJECT. It never blocks progression
// itself — review_plan's wait_human step already requires an explicit human
// action every time regardless of verdict — this only makes sure "the critic
// asked for changes" is visible right where that decision gets made, instead
// of requiring the human to open and read the full critique sidecar.
// A missing/malformed structured verdict never silently behaves like APPROVE:
// it bounded-retries the critique step with targeted schema feedback, then
// escalates to human-required once the retry budget is spent.
func (e *Engine) execFlagPlanCritique(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	verdict := planCritiqueVerdict(t)
	switch verdict {
	case planCritiqueVerdictApprove, "":
		if verdict == "" {
			return e.retryOrEscalateMalformedPlanCritique(taskID, step, wfExec, t)
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "verdict: " + verdict}, nil
	case planCritiqueVerdictRefine, planCritiqueVerdictReject:
	default:
		return e.retryOrEscalateMalformedPlanCritique(taskID, step, wfExec, t)
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

func (e *Engine) retryOrEscalateMalformedPlanCritique(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	sourceStep := wfExec.Variables[planCritiqueVerdictSourceStepVar]
	if sourceStep == "" {
		sourceStep = planCritiqueSourceStep
	}
	armed, attempt, err := e.rewindRetry(taskID, wfExec, t, rewindRetryPolicy{
		counterKey: "step." + sourceStep + ".plan_critique_verdict_retry",
		max:        planCritiqueVerdictAutoRetryCap,
		rewindStep: sourceStep,
		backoff:    func(int) time.Duration { return planCritiqueVerdictAutoRetryBackoff },
		onArm: func(wfExec *Execution, _ int) {
			wfExec.SetVar(planCritiqueReaskNoteVar, planCritiqueMalformedReask)
			delete(wfExec.Variables, planCritiqueVerdictVar)
		},
		reason: func(attempt int) string {
			return fmt.Sprintf("re-running plan critique (attempt %d/%d): previous response did not return a schema-valid verdict",
				attempt, planCritiqueVerdictAutoRetryCap)
		},
	})
	if err != nil {
		return StepOutput{}, err
	}
	if armed {
		e.logger.Warn("workflow.plan-critique.malformed-verdict.retry", "task_id", taskID, "step", sourceStep, "attempt", attempt)
		return StepOutput{}, errStepParked
	}
	reason := "plan critique did not return a schema-valid verdict after auto-retries — needs human inspection of the critique sidecar"
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Warn("workflow.plan-critique.malformed-verdict.escalate", "task_id", taskID, "step", sourceStep)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "malformed verdict — escalated"}, nil
}

func planCritiqueVerdict(t TaskInfo) string {
	if t.Workflow != nil {
		if verdict, ok := t.Workflow.Variables[planCritiqueVerdictVar]; ok {
			return normalizePlanCritiqueVerdict(verdict)
		}
		if _, ok := t.Workflow.Variables[planCritiqueVerdictSourceStepVar]; ok {
			return ""
		}
	}
	return parsePlanCritiqueVerdict(t.PlanCritique)
}

func ExtractPlanCritiqueVerdict(output string) string {
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var v struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	return normalizePlanCritiqueVerdict(v.Verdict)
}

func normalizePlanCritiqueVerdict(verdict string) string {
	switch canonicalVerdict(verdict) {
	case planCritiqueVerdictApprove:
		return planCritiqueVerdictApprove
	case planCritiqueVerdictRefine:
		return planCritiqueVerdictRefine
	case planCritiqueVerdictReject:
		return planCritiqueVerdictReject
	default:
		return ""
	}
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
