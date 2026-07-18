package workflow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var prFixSentinelRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_RESULT:\s*([a-z_-]+)\s*$`)
var prFixReasonRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_REASON:\s*(.+?)\s*$`)

// prFixFailingTestRe matches each repeated SYBRA_PR_FIX_FAILING_TEST: line a
// pr-fix agent emits alongside a human-required verdict for non-merge test
// failures it already found while investigating. One sentinel per test,
// unlike RESULT/REASON, so every named test survives — not just the last.
var prFixFailingTestRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_FAILING_TEST:\s*(.+?)\s*$`)

// ReviewHoldParkVar, when set to "true" on the execution, forces the pr-fix
// result to human-required regardless of the agent's sentinel. It's a
// deterministic backstop for the review-hold setting: the fix-review agent
// drafted its replies into a pending review, so the task must park for a human
// to submit even if the agent (correctly, in push mode) reported that it pushed
// and would otherwise route to `continue`. Set by the dispatcher when the hold
// is active and the handled issue set includes review comments.
const ReviewHoldParkVar = "review_hold_park"

const reviewHoldParkReason = "review-hold: replies drafted as a pending review — verify & submit on GitHub"

// PRFixVerdict is the outcome a pr-fix agent reported via its SYBRA_PR_FIX_RESULT sentinel.
type PRFixVerdict string

// PRFixContinue means the agent pushed a fix.
const PRFixContinue PRFixVerdict = "continue"

// PRFixFlake means the diff is correct and CI failed for unrelated reasons.
const PRFixFlake PRFixVerdict = "flake"

// PRFixHuman means the agent intentionally stopped for a human.
const PRFixHuman PRFixVerdict = "human-required"

// PRFixVerdictVar is the execution variable holding a pr-fix step's PRFixVerdict.
const PRFixVerdictVar = "pr_fix_verdict"

func (e *Engine) execRoutePRFixResult(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	verdict, reason := prFixVerdict(wfExec)
	// In push mode the agent pushed and reports `continue`, which must NOT release the hold. EXC:FILE011:load-bearing-invariant
	reviewHoldForced := false
	if verdict != PRFixHuman && wfExec != nil && wfExec.Variables[ReviewHoldParkVar] == "true" {
		verdict = PRFixHuman
		reason = reviewHoldParkReason
		reviewHoldForced = true
	}
	if verdict == PRFixContinue {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "continue"}, nil
	}
	// A flake has no commit to verify, so parking or verify_commits would punish the honest answer. EXC:FILE011:load-bearing-invariant
	if verdict == PRFixFlake {
		msg := "pr-fix: CI failure unrelated to this PR"
		if reason != "" {
			msg += ": " + reason
		}
		if err := e.tasks.UpdateTaskStatus(taskID, "in-review", msg); err != nil {
			return StepOutput{}, fmt.Errorf("route pr-fix result: set in-review after flake: %w", err)
		}
		e.logger.Info("workflow.pr-fix.flake", "task_id", taskID, "pr", t.PRNumber, "reason", reason)
		return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
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
	// Best-effort, after the status update: the failing-tests note is a
	// supplement to a park that has already happened, not a precondition for
	// it, so a store failure here must never leave the task un-parked or
	// re-append the note on a retry of this step. reviewHoldForced means the
	// agent's own verdict was continue/flake — any SYBRA_PR_FIX_FAILING_TEST:
	// lines in its output describe tests it already dealt with, not the
	// reason for this park, so they must not be attributed to it.
	if !reviewHoldForced {
		if tests := prFixFailingTests(wfExec); len(tests) > 0 {
			note := "## PR-Fix: Failing Tests\n\n" + reason + "\n\n- " + strings.Join(tests, "\n- ")
			if err := e.tasks.AppendTaskBody(taskID, note); err != nil {
				e.logger.Warn("workflow.pr-fix.failing-tests.append", "task_id", taskID, "err", err)
			}
		}
	}
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

// recordPRFixVars classifies a just-completed pr-fix agent's raw output and
// stashes the verdict/reason/failing-tests as step vars, so prFixVerdict and
// prFixFailingTests can resolve them without re-classifying on every lookup.
// Split out of AdvanceStep to keep that function under the funlen limit.
func recordPRFixVars(wfExec *Execution, stepID, output string) {
	verdict, reason := classifyPRFixResult(output)
	wfExec.SetVar("step."+stepID+"."+PRFixVerdictVar, string(verdict))
	wfExec.SetVar("step."+stepID+".pr_fix_requires_human", strconv.FormatBool(verdict == PRFixHuman))
	wfExec.SetVar("step."+stepID+".pr_fix_reason", reason)
	// Always set (possibly to ""), never skip: a stale value from an earlier
	// completion of this same step ID must not survive to be misread as this
	// attempt's failing tests by prFixFailingTests. See prFixFailingTests'
	// ok-but-empty branch, which depends on this var actually being present.
	var failingTests string
	if verdict == PRFixHuman {
		failingTests = strings.Join(extractPRFixFailingTests(output), "\n")
	}
	wfExec.SetVar("step."+stepID+".pr_fix_failing_tests", failingTests)
}

func prFixVerdict(wfExec *Execution) (verdict PRFixVerdict, reason string) {
	if wfExec == nil {
		return PRFixContinue, ""
	}
	stepID := wfExec.LastAgentStepID()
	if stepID == "" {
		return PRFixContinue, ""
	}
	if wfExec.Variables != nil {
		if v := wfExec.Variables["step."+stepID+"."+PRFixVerdictVar]; v != "" {
			switch PRFixVerdict(v) {
			case PRFixHuman, PRFixFlake:
				return PRFixVerdict(v), wfExec.Variables["step."+stepID+".pr_fix_reason"]
			case PRFixContinue:
				return PRFixContinue, ""
			}
		}
		// An execution advanced by a pre-flake binary carries only the boolean. EXC:FILE011:load-bearing-invariant
		switch wfExec.Variables["step."+stepID+".pr_fix_requires_human"] {
		case "true":
			return PRFixHuman, wfExec.Variables["step."+stepID+".pr_fix_reason"]
		case "false":
			return PRFixContinue, ""
		}
	}
	return classifyPRFixResult(lastPRFixOutput(wfExec, stepID))
}

// prFixFailingTests returns the specific failing tests a pr-fix agent named
// via repeated SYBRA_PR_FIX_FAILING_TEST: lines alongside a human-required
// verdict — the concrete repro info a human (or a future scoped test-fix
// follow-up) needs, instead of just the free-text reason, which truncate()s
// to 200 chars and would otherwise drop it. Mirrors prFixVerdict's own
// var-first-then-reclassify lookup so both stay consistent across a resume.
func prFixFailingTests(wfExec *Execution) []string {
	if wfExec == nil {
		return nil
	}
	stepID := wfExec.LastAgentStepID()
	if stepID == "" {
		return nil
	}
	if wfExec.Variables != nil {
		if v, ok := wfExec.Variables["step."+stepID+".pr_fix_failing_tests"]; ok {
			if v == "" {
				return nil
			}
			return strings.Split(v, "\n")
		}
	}
	return extractPRFixFailingTests(lastPRFixOutput(wfExec, stepID))
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

func classifyPRFixResult(output string) (verdict PRFixVerdict, reason string) {
	if strings.TrimSpace(output) == "" {
		return PRFixContinue, ""
	}
	matches := prFixSentinelRe.FindAllStringSubmatch(output, -1)
	if len(matches) > 0 {
		m := matches[len(matches)-1]
		switch strings.ToLower(strings.TrimSpace(m[1])) {
		case "human-required", "human_required", "human":
			return PRFixHuman, extractPRFixReason(output)
		case "flake", "no-op", "no_op", "noop":
			return PRFixFlake, sentinelReason(output)
		case "continue", "ok", "done":
			return PRFixContinue, ""
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
			return PRFixContinue, ""
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
			return PRFixHuman, "pr-fix agent requested human review: " + truncate(firstNonEmptyLine(output), 200)
		}
	}
	return PRFixContinue, ""
}

// Empty when the agent gave no reason; only human-required defaults its text. EXC:FILE011:load-bearing-invariant
func sentinelReason(output string) string {
	matches := prFixReasonRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return ""
	}
	m := matches[len(matches)-1]
	return truncate(strings.TrimSpace(m[1]), 200)
}

func extractPRFixReason(output string) string {
	if reason := sentinelReason(output); reason != "" {
		return reason
	}
	return "pr-fix agent requested human review"
}

// extractPRFixFailingTests collects every SYBRA_PR_FIX_FAILING_TEST: line
// from a pr-fix agent's output, in the order emitted, untruncated.
func extractPRFixFailingTests(output string) []string {
	matches := prFixFailingTestRe.FindAllStringSubmatch(output, -1)
	tests := make([]string, 0, len(matches))
	for _, m := range matches {
		if t := strings.TrimSpace(m[1]); t != "" {
			tests = append(tests, t)
		}
	}
	return tests
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
