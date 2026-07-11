package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

var prFixSentinelRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_RESULT:\s*([a-z_-]+)\s*$`)
var prFixReasonRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_REASON:\s*(.+?)\s*$`)

// ReviewHoldParkVar, when set to "true" on the execution, forces the pr-fix
// result to human-required regardless of the agent's sentinel. It's a
// deterministic backstop for the review-hold setting: the fix-review agent
// drafted its replies into a pending review, so the task must park for a human
// to submit even if the agent (correctly, in push mode) reported that it pushed
// and would otherwise route to `continue`. Set by the dispatcher when the hold
// is active and the handled issue set includes review comments.
const ReviewHoldParkVar = "review_hold_park"

const reviewHoldParkReason = "review-hold: replies drafted as a pending review — verify & submit on GitHub"

func (e *Engine) execRoutePRFixResult(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	requiresHuman, reason := prFixRequiresHuman(wfExec)
	// Deterministic park wins over the sentinel: in push mode the agent pushed
	// and its own sentinel says `continue`, which must NOT release the hold.
	reviewHoldForced := false
	if !requiresHuman && wfExec != nil && wfExec.Variables[ReviewHoldParkVar] == "true" {
		requiresHuman = true
		reason = reviewHoldParkReason
		reviewHoldForced = true
	}
	if !requiresHuman {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "continue"}, nil
	}
	// The review-hold park exists because a pending review draft needs a human
	// to submit it — that's true regardless of the remote PR's CI/mergeable
	// state, so it must never be waved through by the re-probe below.
	if !reviewHoldForced {
		if msg, resolved := e.checkPRAlreadyResolved(taskID, t, reason); resolved {
			if err := e.tasks.UpdateTaskStatus(taskID, "in-review", msg); err != nil {
				return StepOutput{}, fmt.Errorf("route pr-fix result: resolved-on-remote: set in-review: %w", err)
			}
			e.logger.Info("workflow.pr-fix.resolved-on-remote", "task_id", taskID, "pr", t.PRNumber, "agent_reason", reason)
			return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
		}
	}
	if reason == "" {
		reason = "pr-fix agent requested human review"
	}
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("route pr-fix result: set human-required: %w", err)
	}
	e.logger.Warn("workflow.pr-fix.human-required", "task_id", taskID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

// checkPRAlreadyResolved re-probes the live PR when the pr-fix agent (or a
// pre-agent rebase-block) requested human review — the agent may have
// correctly declined to push because its local worktree was stale/diverged
// from a remote branch an external bot already force-fixed. A pending run
// must never count as resolved, so any fetch error falls through to the
// normal human-required park.
func (e *Engine) checkPRAlreadyResolved(taskID string, t TaskInfo, agentReason string) (msg string, resolved bool) {
	if e.prStates == nil || t.ProjectID == "" || t.PRNumber <= 0 {
		return "", false
	}
	state, err := e.prStates.FetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.pr-fix.resolved-probe-failed", "task_id", taskID, "pr", t.PRNumber, "err", err)
		return "", false
	}
	if !state.Resolved() {
		return "", false
	}
	if agentReason == "" {
		agentReason = "pr-fix agent requested human review"
	}
	return fmt.Sprintf("pr-fix skipped park: PR #%d already resolved on remote (agent reported: %s)", t.PRNumber, agentReason), true
}

func prFixRequiresHuman(wfExec *Execution) (requiresHuman bool, reason string) {
	if wfExec == nil {
		return false, ""
	}
	stepID := wfExec.LastAgentStepID()
	if stepID == "" {
		return false, ""
	}
	if wfExec.Variables != nil {
		switch wfExec.Variables["step."+stepID+".pr_fix_requires_human"] {
		case "true":
			return true, wfExec.Variables["step."+stepID+".pr_fix_reason"]
		case "false":
			return false, ""
		}
	}
	return classifyPRFixResult(lastPRFixOutput(wfExec, stepID))
}

func lastPRFixOutput(wfExec *Execution, stepID string) string {
	if wfExec == nil || stepID == "" {
		return ""
	}
	if rec := wfExec.RecordForStep(stepID); rec != nil {
		return rec.Output
	}
	if wfExec.Variables != nil {
		return wfExec.Variables["step."+stepID+".output"]
	}
	return ""
}

func classifyPRFixResult(output string) (requiresHuman bool, reason string) {
	if strings.TrimSpace(output) == "" {
		return false, ""
	}
	matches := prFixSentinelRe.FindAllStringSubmatch(output, -1)
	if len(matches) > 0 {
		m := matches[len(matches)-1]
		switch strings.ToLower(strings.TrimSpace(m[1])) {
		case "human-required", "human_required", "human":
			return true, extractPRFixReason(output)
		case "continue", "ok", "done":
			return false, ""
		}
	}

	lower := strings.ToLower(output)
	negativePhrases := []string{
		"no human review is required",
		"does not require human",
		"doesn't require human",
		"without human review",
	}
	for _, phrase := range negativePhrases {
		if strings.Contains(lower, phrase) {
			return false, ""
		}
	}

	humanPhrases := []string{
		"human review is required",
		"requires human review",
		"require human review",
		"needs human review",
		"human intervention is required",
		"requires human intervention",
		"requires manual resolution",
		"manual resolution is required",
		"manually resolve",
		"too many conflicts",
		"more than 3 conflicting files",
		"exceeds the 3-file limit",
		"exceeds the limit of 3",
		"this task requires human review",
	}
	for _, phrase := range humanPhrases {
		if strings.Contains(lower, phrase) {
			return true, "pr-fix agent requested human review: " + truncate(firstNonEmptyLine(output), 200)
		}
	}
	return false, ""
}

func extractPRFixReason(output string) string {
	matches := prFixReasonRe.FindAllStringSubmatch(output, -1)
	if len(matches) > 0 {
		m := matches[len(matches)-1]
		return truncate(strings.TrimSpace(m[1]), 200)
	}
	return "pr-fix agent requested human review"
}

func firstNonEmptyLine(s string) string {
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
