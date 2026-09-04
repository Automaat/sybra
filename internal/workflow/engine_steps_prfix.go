package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/errclass"
	"github.com/Automaat/sybra/internal/prepstate"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
)

var prFixSentinelRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_RESULT:\s*([a-z_-]+)\s*$`)

// prFixReasonLineRe locates where a SYBRA_PR_FIX_REASON: value begins. It is
// intentionally a prefix match, not a single-line capture: a real diagnosis
// is routinely more than one line, and `$` in (?m) mode ends at the next
// newline, so a capturing group here would silently drop everything past
// line 1 — see sentinelReason for how the value's end is actually found.
var prFixReasonLineRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_REASON:\s*`)

// prFixSentinelKeyRe matches the start of any SYBRA_PR_FIX_* sentinel line,
// used to find where a multi-line REASON value ends: at the next sentinel
// line, or end of output if there is none.
var prFixSentinelKeyRe = regexp.MustCompile(`(?im)^SYBRA_PR_FIX_(?:RESULT|REASON|FAILING_TEST):`)

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

// routePRFixResultStepID is pr-fix.yaml's (and branch-conflict-fix.yaml's)
// step ID for the original router — the only one allowed to redirect to
// test_fix, so route_test_fix_result's own routing of test_fix's completion
// can never loop back into another test-fix attempt.
const routePRFixResultStepID = "route_pr_fix_result"

// prFixTestFixEligibleVar is the step var routePRFixResultStepID sets to
// "true" when redirecting to test_fix instead of parking, so pr-fix.yaml's
// `next` transitions can branch on it with a single equality check.
const prFixTestFixEligibleVar = "pr_fix_test_fix_eligible"

const resolvedMergeCheckpointedVar = "resolved_merge_checkpointed"

const resolvedMergeCheckpointMessage = "fix(recovery): finalize merge resolution\n\nroute_pr_fix_result recovered a marker-free merge the pr-fix agent resolved on disk but never staged/committed."

// PRFixVerdict is the outcome a pr-fix agent reported via its SYBRA_PR_FIX_RESULT sentinel.
type PRFixVerdict string

// PRFixContinue means the agent pushed a fix.
const PRFixContinue PRFixVerdict = "continue"

// PRFixFlake means the diff is correct and CI failed for unrelated reasons.
const PRFixFlake PRFixVerdict = "flake"

// PRFixNoop means the live PR no longer has an issue for this run to fix.
const PRFixNoop PRFixVerdict = "no-op"

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
		e.recordPreparedWorktreeState(taskID, wfExec, t)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "continue"}, nil
	}
	if verdict == PRFixNoop {
		msg := "pr-fix: live PR no longer requires changes"
		if reason != "" {
			msg += ": " + reason
		}
		if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.InReview, msg); err != nil {
			return StepOutput{}, fmt.Errorf("route pr-fix result: set in-review after no-op: %w", err)
		}
		e.logger.Info("workflow.pr-fix.no-op", "task_id", taskID, "pr", t.PRNumber, "reason", reason)
		return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
	}
	// A flake has no commit to verify, so parking or verify_commits would punish the honest answer. EXC:FILE011:load-bearing-invariant
	if verdict == PRFixFlake {
		msg := "pr-fix: CI failure unrelated to this PR"
		if reason != "" {
			msg += ": " + reason
		}
		if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.InReview, msg); err != nil {
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
			if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.InReview, msg); err != nil {
				return StepOutput{}, fmt.Errorf("route pr-fix result: resolved-on-remote: set in-review: %w", err)
			}
			e.logger.Info("workflow.pr-fix.resolved-on-remote", "task_id", taskID, "pr", t.PRNumber, "agent_reason", reason)
			return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
		}
	}
	if reason == "" {
		reason = "pr-fix agent requested human review"
	}
	// A bounded, single-shot follow-up: only route_pr_fix_result (the
	// original router, gated by step ID so route_test_fix_result's own
	// human-required outcome — test_fix's own attempt — can never loop back
	// here) offers the scoped test_fix step a shot at the specific failing
	// tests pr-fix already found, before parking a human. reviewHoldForced
	// still bypasses this: a pending review draft needs a human regardless.
	if !reviewHoldForced && step.ID == routePRFixResultStepID {
		if tests := prFixFailingTests(wfExec); len(tests) > 0 {
			wfExec.SetVar("step."+step.ID+"."+prFixTestFixEligibleVar, "true")
			e.logger.Info("workflow.pr-fix.test-fix-eligible", "task_id", taskID, "pr", t.PRNumber, "test_count", len(tests))
			return StepOutput{StepID: step.ID, Status: "completed", Output: "routing to scoped test-fix: " + reason}, nil
		}
	}
	if !reviewHoldForced && prFixAllowsResolvedMergeRecovery(reason) {
		if out, err, handled := e.tryRecoverResolvedMerge(taskID, step, wfExec, t); handled {
			return out, err
		}
	}
	if !reviewHoldForced && prFixShouldResumeNoPRRecovery(t, reason) {
		msg := "retryable no-PR remote outage; resuming original workflow: " + reason
		workflowID := ""
		if wfExec != nil {
			workflowID = wfExec.WorkflowID
		}
		e.logger.Warn("workflow.pr-fix.no-pr-retryable-remote",
			"task_id", taskID, "workflow", workflowID, "reason", reason)
		return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
	}
	if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); err != nil {
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

func prFixShouldResumeNoPRRecovery(t TaskInfo, reason string) bool {
	if t.PRNumber > 0 {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	return errclass.Classify(reason, errclass.PRFixProseRetryBiased) == errclass.Transient
}

func (e *Engine) recordPreparedWorktreeState(taskID string, wfExec *Execution, t TaskInfo) {
	if wfExec == nil || wfExec.WorkflowID != "branch-conflict-fix" {
		return
	}
	dir := wfExec.Variables[WorkflowVarDir]
	if dir == "" {
		return
	}
	branch := t.Branch
	if branch == "" {
		branch = strings.TrimSpace(t.Branch)
	}
	wrote, err := prepstate.WriteVerified(e.ctx, dir, branch)
	if err != nil {
		e.logger.Warn("workflow.pr-fix.prep-state-write", "task_id", taskID, "workflow", wfExec.WorkflowID, "dir", dir, "branch", branch, "err", err)
		return
	}
	if wrote {
		e.logger.Info("workflow.pr-fix.prep-state-written", "task_id", taskID, "workflow", wfExec.WorkflowID, "dir", dir, "branch", branch)
	}
}

func prFixAllowsResolvedMergeRecovery(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return false
	}
	for _, blocker := range []string{"approval", "base-only", "no substantive", "do not push", "without pushing", "declining to push"} {
		if strings.Contains(reason, blocker) {
			return false
		}
	}
	for _, signal := range []string{"unmerged", "merge not finalized", "merge still", "merge in progress"} {
		if strings.Contains(reason, signal) {
			return true
		}
	}
	return strings.Contains(reason, "merge conflict") && strings.Contains(reason, "git")
}

func (e *Engine) tryRecoverResolvedMerge(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error, bool) {
	if e.execution.Worktrees == nil {
		return StepOutput{}, nil, false
	}
	wtPath, ok := e.execution.Worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{}, nil, false
	}
	if wfExec != nil && wfExec.Variables[resolvedMergeCheckpointVar(step.ID)] == "true" {
		return e.pushRecoveredResolvedMergeCommit(taskID, step, wfExec, t, wtPath)
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	resolved, err := project.ResolvedUnmergedPaths(ctx, wtPath)
	if err != nil {
		e.logger.Warn("workflow.pr-fix.recover-resolved-merge.detect", "task_id", taskID, "err", err)
		return StepOutput{}, nil, false
	}
	if len(resolved) == 0 {
		return StepOutput{}, nil, false
	}

	cmds, checkedFiles, err := e.resolvedMergeFocusedCommands(ctx, taskID, wtPath, resolved)
	if err != nil {
		out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict could not determine a focused sanity gate: "+trimDiffLine(err.Error()))
		return out, err, true
	}
	if len(cmds) == 0 {
		return StepOutput{}, nil, false
	}

	timeout := e.verifyTimeout
	if timeout <= 0 {
		timeout = verifyChecksDefaultTimeout
	}
	checkCtx, checkCancel := context.WithTimeout(e.ctx, timeout)
	defer checkCancel()
	maybeMiseTrust(checkCtx, wtPath)
	failedCmd, output, runErr := e.runVerifyCommands(checkCtx, taskID, wtPath, cmds)
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			out, err := e.humanRequiredPR(taskID, step, fmt.Sprintf("resolved merge conflict sanity gate exceeded the time budget (%s)", timeout))
			return out, err, true
		}
		if errors.Is(runErr, context.Canceled) && e.ctx.Err() != nil {
			e.logger.Warn("workflow.pr-fix.recover-resolved-merge.canceled", "task_id", taskID, "err", runErr)
			out, err := stepDone(step, "skipped: context canceled")
			return out, err, true
		}
		out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict sanity gate could not run cleanly: "+trimDiffLine(runErr.Error()))
		return out, err, true
	}
	if failedCmd != "" {
		reason := "resolved merge conflict sanity gate failed: " + trimDiffLine(failedCmd)
		if tail := strings.TrimSpace(tailString(output, 500)); tail != "" {
			reason += " (" + tail + ")"
		}
		out, err := e.humanRequiredPR(taskID, step, reason)
		return out, err, true
	}

	if out, err, handled := e.rejectUnexpectedResolvedMergeDirtyPaths(taskID, step, wtPath, checkedFiles); handled {
		return out, err, true
	}

	commitCtx, commitCancel := context.WithTimeout(e.ctx, shellTimeout)
	defer commitCancel()
	committed, err := project.CheckpointCommit(commitCtx, wtPath, resolvedMergeCheckpointMessage)
	if err != nil {
		out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict could not be checkpoint-committed: "+trimDiffLine(err.Error()))
		return out, err, true
	}
	if !committed {
		out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict left no checkpointable changes after recovery")
		return out, err, true
	}
	if wfExec != nil {
		wfExec.SetVar(resolvedMergeCheckpointVar(step.ID), "true")
		if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
			out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict commit landed but recovery state could not be persisted: "+trimDiffLine(err.Error()))
			return out, err, true
		}
	}

	return e.pushRecoveredResolvedMergeCommit(taskID, step, wfExec, t, wtPath)
}

func (e *Engine) pushRecoveredResolvedMergeCommit(taskID string, step *Step, wfExec *Execution, t TaskInfo, wtPath string) (StepOutput, error, bool) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	branch := t.Branch
	if branch == "" {
		var err error
		branch, err = project.CurrentBranch(ctx, wtPath)
		if err != nil || branch == "" {
			reason := "resolved merge conflict commit landed but the task branch could not be determined"
			if err != nil {
				reason += ": " + trimDiffLine(err.Error())
			}
			out, err := e.humanRequiredPR(taskID, step, reason)
			return out, err, true
		}
	}
	if out, err, ok := e.pushTaskBranch(taskID, step, wfExec, t, wtPath, branch); !ok {
		return out, err, true
	}
	if e.pr.HeadFetcher != nil && t.PRNumber > 0 && t.ProjectID != "" {
		e.verifyPushedHead(taskID, wtPath, t)
	}
	e.logger.Info("workflow.pr-fix.recovered-resolved-merge", "task_id", taskID, "branch", branch)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "continue"}, nil, true
}

func resolvedMergeCheckpointVar(stepID string) string {
	return "step." + stepID + "." + resolvedMergeCheckpointedVar
}

func (e *Engine) rejectUnexpectedResolvedMergeDirtyPaths(taskID string, step *Step, wtPath string, checkedFiles []string) (StepOutput, error, bool) {
	unexpectedDirty, err := resolvedMergeUnexpectedDirtyPaths(e.ctx, wtPath, checkedFiles)
	if err != nil {
		out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict dirty-path audit failed: "+trimDiffLine(err.Error()))
		return out, err, true
	}
	if len(unexpectedDirty) == 0 {
		return StepOutput{}, nil, false
	}
	out, err := e.humanRequiredPR(taskID, step, "resolved merge conflict left dirty paths outside the focused sanity gate: "+trimDiffLine(strings.Join(unexpectedDirty, ", ")))
	return out, err, true
}

func (e *Engine) resolvedMergeFocusedCommands(ctx context.Context, taskID, wtPath string, resolved []string) (commands, files []string, err error) {
	files = slices.Clone(resolved)
	changed, err := changedFilesSinceProjectBase(ctx, wtPath, e.focusedChecksBaseRef(taskID))
	if err != nil {
		e.logger.Warn("workflow.pr-fix.recover-resolved-merge.changed-files", "task_id", taskID, "err", err)
		return nil, nil, err
	}
	for _, file := range changed {
		if !slices.Contains(files, file) {
			files = append(files, file)
		}
	}
	_, commands = selectFocusedChecks(e.execution.Checks.FocusedChecks(ctx, taskID), files)
	return commands, files, nil
}

func resolvedMergeUnexpectedDirtyPaths(ctx context.Context, wtPath string, allowed []string) ([]string, error) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, file := range allowed {
		clean, ok := cleanGitRelPath(file)
		if !ok {
			return nil, fmt.Errorf("unsafe focused path %q", file)
		}
		allowedSet[clean] = struct{}{}
	}

	dirty, err := gitStatusPorcelainPaths(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	var unexpected []string
	for _, file := range dirty {
		clean, ok := cleanGitRelPath(file)
		if !ok {
			return nil, fmt.Errorf("unsafe dirty path %q", file)
		}
		if _, ok := allowedSet[clean]; !ok {
			unexpected = append(unexpected, clean)
		}
	}
	return unexpected, nil
}

func gitStatusPorcelainPaths(ctx context.Context, wtPath string) ([]string, error) {
	out, err := gitCombinedOutput(ctx, wtPath, "status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}

	var paths []string
	parts := bytes.Split(out, []byte{0})
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) == 0 {
			continue
		}
		if len(entry) < 4 {
			return nil, fmt.Errorf("parse git status --porcelain -z entry %q", string(entry))
		}
		paths = append(paths, string(entry[3:]))
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
		}
	}
	return paths, nil
}

func cleanGitRelPath(file string) (string, bool) {
	if file == "" || path.IsAbs(file) {
		return "", false
	}
	clean := path.Clean(file)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// checkPRAlreadyResolved re-probes the live PR when the pr-fix agent (or a
// pre-agent rebase-block) requested human review — the agent may have
// correctly declined to push because its local worktree was stale/diverged
// from a remote branch an external bot already force-fixed. A pending run
// must never count as resolved, so any fetch error falls through to the
// normal human-required park.
func (e *Engine) checkPRAlreadyResolved(taskID string, t TaskInfo, agentReason string) (msg string, resolved bool) {
	if e.pr.StateFetcher == nil || t.ProjectID == "" || t.PRNumber <= 0 {
		return "", false
	}
	state, err := e.pr.StateFetcher.FetchPRState(t.ProjectID, t.PRNumber)
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
			case PRFixHuman, PRFixFlake, PRFixNoop:
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
// verdict — structured repro info a human (or a future scoped test-fix
// follow-up) can consume directly, instead of re-parsing it out of the
// free-text reason. Mirrors prFixVerdict's own var-first-then-reclassify
// lookup so both stay consistent across a resume.
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
		case "flake":
			return PRFixFlake, sentinelReason(output)
		case "no-op", "no_op", "noop":
			return PRFixNoop, sentinelReason(output)
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
			return PRFixHuman, "pr-fix agent requested human review: " + strings.TrimSpace(output)
		}
	}
	return PRFixContinue, ""
}

// Empty when the agent gave no reason; only human-required defaults its text. EXC:FILE011:load-bearing-invariant
// sentinelReason returns the last SYBRA_PR_FIX_REASON: value, spanning as
// many lines as the agent wrote — the value ends at the next SYBRA_PR_FIX_*
// sentinel line, or at the end of output if there is none. RE2 has no
// lookahead, so the end boundary is found with a second, separate search
// rather than a single capturing regex.
func sentinelReason(output string) string {
	starts := prFixReasonLineRe.FindAllStringIndex(output, -1)
	if len(starts) == 0 {
		return ""
	}
	valueStart := starts[len(starts)-1][1]
	value := output[valueStart:]
	if end := prFixSentinelKeyRe.FindStringIndex(value); end != nil {
		value = value[:end[0]]
	}
	return strings.TrimSpace(value)
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
