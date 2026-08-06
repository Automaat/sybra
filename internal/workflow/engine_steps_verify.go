package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/errclass"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
)

// errStepParked is a sentinel returned by a synchronous step that has parked
// its own workflow in ExecWaiting (persisting the new CurrentStep/State itself).
// executeSteps treats it as "stop, do not record/advance/complete" — completing
// would fire the status-change cascade. Used by verify_commits to wait out a
// still-running sibling agent without launching over it.
var errStepParked = errors.New("workflow step parked in ExecWaiting")

const (
	finalCommitSourceAgent    = "agent"
	finalCommitSourceFallback = "fallback"
)

// execEnsurePRClosesIssue verifies the task's PR closes its linked
// GitHub issue. When the closing reference is missing, it appends
// `Closes <issue-url>` to the PR body via the PRLinker and re-verifies.
// On verification failure the task is flipped to human-required so a
// human can fix the linkage manually.
//
// The step is a no-op when any of these are missing: task.Issue,
// task.PRNumber, task.ProjectID, engine.prLinker. It also skips when
// the issue lives in a different repo than the PR (cross-repo linking
// needs explicit support GitHub handles but this check does not).
func (e *Engine) execEnsurePRClosesIssue(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.prLinker == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no pr linker configured"}, nil
	}
	if t.Issue == "" || t.PRNumber == 0 || t.ProjectID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: missing issue, pr, or project"}, nil
	}

	issueRepo, issueNum := github.ParseIssueURL(t.Issue)
	if issueNum == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: unparseable issue url"}, nil
	}
	if issueRepo != t.ProjectID {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: cross-repo issue link"}, nil
	}

	issues, body, err := e.prLinker.GetClosingIssues(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Error("workflow.pr-close.fetch", "task_id", taskID, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "fetch failed: " + err.Error()}, nil
	}
	if slices.Contains(issues, issueNum) {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "already linked"}, nil
	}

	newBody := body
	if newBody != "" {
		newBody += "\n\n"
	}
	newBody += "Closes " + t.Issue
	if editErr := e.prLinker.EditBody(t.ProjectID, t.PRNumber, newBody); editErr != nil {
		e.logger.Error("workflow.pr-close.edit", "task_id", taskID, "err", editErr)
		if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, "PR does not close linked issue and auto-fix failed: "+editErr.Error()); statusErr != nil {
			e.logger.Error("workflow.pr-close.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "failed", Output: "edit failed: " + editErr.Error()}, nil
	}

	// Verify with retry — GitHub updates closingIssuesReferences
	// asynchronously after a body edit, so the first fetch can miss
	// refs that populate seconds later. If every retry still misses,
	// trust the body: we just wrote "Closes <url>" into it with a
	// known-good format, so the link will resolve once GitHub catches
	// up. Only edit failures (above) flip to human-required.
	var verifyErr error
	for attempt := 0; attempt <= len(prVerifyBackoffs); attempt++ {
		if attempt > 0 {
			prVerifySleep(prVerifyBackoffs[attempt-1])
		}
		var verified []int
		verified, _, verifyErr = e.prLinker.GetClosingIssues(t.ProjectID, t.PRNumber)
		if verifyErr == nil && slices.Contains(verified, issueNum) {
			e.logger.Info("workflow.pr-close.linked", "task_id", taskID, "pr", t.PRNumber, "issue", issueNum, "attempt", attempt)
			return StepOutput{StepID: step.ID, Status: "completed", Output: fmt.Sprintf("linked issue #%d", issueNum)}, nil
		}
	}

	e.logger.Warn("workflow.pr-close.verify-lag", "task_id", taskID, "pr", t.PRNumber, "issue", issueNum, "err", verifyErr)
	msg := fmt.Sprintf("edited body to close #%d; verification lagged — trusting body contents", issueNum)
	if verifyErr != nil {
		msg = fmt.Sprintf("edited body to close #%d; last verify err: %s — trusting body contents", issueNum, verifyErr.Error())
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
}

// execStampPRAttribution appends the harness attribution footer to the task's
// PR body, mirroring the deterministic floor applied to Sybra-authored issue/PR
// comments (internal/attribution). It is the machine-side guarantee that every
// Sybra-opened PR is identifiable as harness-generated, independent of whether
// the LLM-drafted PR body (internal/prcontent, via the create_pr step)
// happened to include it.
//
// It runs after ensure_pr_closes_issue so the footer lands as the last line,
// below any `Closes <issue>` reference. attribution.Append is idempotent, so
// re-runs (retries, re-pushes) never stack duplicate footers. The step is a
// no-op when the PR linker is unset or the task carries no PR/project, and it
// never blocks the workflow — a fetch/edit failure is logged and completed.
func (e *Engine) execStampPRAttribution(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.prLinker == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no pr linker configured"}, nil
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: missing pr or project"}, nil
	}

	_, body, err := e.prLinker.GetClosingIssues(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Error("workflow.pr-attribution.fetch", "task_id", taskID, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "fetch failed: " + err.Error()}, nil
	}

	stamped := attribution.Append(body)
	if stamped == body {
		if strings.TrimSpace(body) == "" {
			return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: empty pr body"}, nil
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "already stamped"}, nil
	}
	if editErr := e.prLinker.EditBody(t.ProjectID, t.PRNumber, stamped); editErr != nil {
		e.logger.Error("workflow.pr-attribution.edit", "task_id", taskID, "err", editErr)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "edit failed: " + editErr.Error()}, nil
	}
	e.logger.Info("workflow.pr-attribution.stamped", "task_id", taskID, "pr", t.PRNumber)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "stamped attribution footer"}, nil
}

func (e *Engine) execRerequestReview(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.prReviewers == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no pr review requester configured"}, nil
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: missing pr or project"}, nil
	}

	reviewers, err := e.prReviewers.RerequestReview(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.rerequest-review.failed", "task_id", taskID, "pr", t.PRNumber, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "request failed: " + err.Error()}, nil
	}
	if len(reviewers) == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no eligible reviewers"}, nil
	}
	msg := "requested review from @" + strings.Join(reviewers, ", @")
	e.logger.Info("workflow.rerequest-review.requested", "task_id", taskID, "pr", t.PRNumber, "reviewers", reviewers)
	return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
}

// execVerifyCommits checks that the task's branch has at least one commit
// ahead of origin/main. This is a non-LLM mechanical gate that runs before
// the eval agent to detect incomplete work without giving eval git access.
//
// Skip conditions (no-op, returns "completed"):
//   - No WorktreeGetter configured
//   - No worktree found for the task
//
// When the branch has no commits ahead of origin/main, OR when the git
// command fails (broken worktree, missing bare clone, unresolvable HEAD),
// the task is flipped to human-required and the step returns "completed" so
// the workflow can route to end via a task.status transition condition.
// Treating git failures as a hard gate prevents the workflow from wasting
// `code_review`/`fix_review`/`create_pr` cycles on a worktree the agent
// cannot operate in.
// execRequireSidecar verifies that the configured sidecar was actually
// written for the task. When empty, flips the task to human-required
// with a descriptive reason instead of silently advancing the workflow.
// Catches the codex-sandbox-blocked class of failure where the agent
// exits cleanly without producing its expected output file.
func (e *Engine) execRequireSidecar(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	var content, label string
	sk := step.Config.Sidecar
	switch {
	case sk == "plan_contract":
		content = t.PlanContract
		label = "plan contract"
	case sk == "plan_critique":
		content = t.PlanCritique
		label = "plan critique"
	case sk == "plan_research":
		content = t.PlanResearch
		label = "plan research"
	case sk == "plan_decisions":
		content = t.PlanDecisions
		label = "plan decisions"
	case sk == "plan_brief":
		content = t.PlanBrief
		label = "plan brief"
	case sk == "code_review":
		content = t.CodeReview
		label = "code review"
	case sk == "plan":
		content = t.Plan
		label = "plan"
	case strings.HasPrefix(sk, "plan_draft."):
		name := strings.TrimPrefix(sk, "plan_draft.")
		content = t.PlanDrafts[name]
		label = "plan draft " + name
	case sk == "":
		return StepOutput{}, fmt.Errorf("require_sidecar: config.sidecar is required")
	default:
		return StepOutput{}, fmt.Errorf("require_sidecar: unknown sidecar %q (want plan|plan_contract|plan_critique|plan_research|plan_decisions|plan_brief|code_review|plan_draft.<name>)", sk)
	}
	if step.Config.AllowMissing && (step.ID != "require_plan_critique" || sk != "plan_critique") {
		return StepOutput{}, fmt.Errorf("require_sidecar: allow_missing is only supported for require_plan_critique plan_critique")
	}
	if strings.TrimSpace(content) == "" {
		reason := label + " missing — upstream agent step completed without writing its sidecar"
		if step.Config.AllowMissing {
			e.logger.Warn("workflow.require-sidecar.missing-soft", "task_id", taskID, "sidecar", step.Config.Sidecar)
			return StepOutput{StepID: step.ID, Status: "completed", Output: reason + " — skipped"}, nil
		}
		if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
			e.logger.Error("workflow.require-sidecar.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.require-sidecar.missing", "task_id", taskID, "sidecar", step.Config.Sidecar)
		return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: label + " present"}, nil
}

// resolveOriginBase returns the remote ref to use as the base for commit
// range comparisons. It first resolves the linked repository's default branch,
// then checks origin/HEAD, followed by compatibility fallbacks. A plain
// rev-parse accepts a syntactically valid but missing object ID, so each
// candidate must resolve to a commit before it can be used in a range.
// Returns "origin/main" if nothing resolves.
func resolveOriginBase(ctx context.Context, wtPath string) string {
	candidates := originBaseCandidates(ctx, wtPath)
	for _, candidate := range candidates {
		if gitOK(ctx, wtPath, "rev-parse", "--verify", candidate+"^{commit}") {
			return candidate
		}
	}
	return "origin/main"
}

func originBaseCandidates(ctx context.Context, wtPath string) []string {
	candidates := make([]string, 0, 4)
	if commonDir, err := gitStdout(ctx, wtPath, "rev-parse", "--git-common-dir"); err == nil && commonDir != "" {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(wtPath, commonDir)
		}
		if bare, err := gitStdout(ctx, commonDir, "rev-parse", "--is-bare-repository"); err == nil && bare == "true" {
			if branch, err := gitStdout(ctx, commonDir, "symbolic-ref", "--short", "HEAD"); err == nil && branch != "" {
				candidates = append(candidates, "origin/"+branch)
			}
		}
	}
	return append(candidates, "origin/HEAD", "origin/master", "origin/main")
}

func (e *Engine) execVerifyCommits(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	// Defense in depth against the duplicate-dispatch class of bug: if a sibling
	// agent (other than the one whose completion triggered this step) is still
	// working the task, the branch may have no commits simply because that agent
	// has not pushed yet. Both flipping to human-required (strands live work) and
	// completing the workflow (its status-change cascade would re-dispatch the
	// implement workflow and StopAgentsForTask the sibling) are wrong. Instead
	// re-arm the implementation run_agent step and park the workflow in
	// ExecWaiting WITHOUT completing it — ResumeStalled re-drives verification
	// once the worktree is quiescent, and the dispatch claim guarantees no
	// duplicate. Excludes the just-completed agent (its done channel closes only
	// after onComplete) so this never fires on the triggering agent itself.
	if err := e.parkVerifyCommitsForSiblingAgent(taskID, wfExec); err != nil {
		return StepOutput{}, err
	}

	output, err := e.gitLogAheadOfBaseWithRetry(taskID, wtPath, t)
	finalSource := ""
	if err != nil {
		if out, ok := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error: "); ok {
			return StepOutput{StepID: step.ID, Status: "completed", Output: out}, nil
		}
	}

	if strings.TrimSpace(string(output)) == "" {
		refreshed, source, out, ok := e.verifyCommitsAfterRemoteAdopt(taskID, wtPath, t)
		if ok {
			output = refreshed
			finalSource = source
		} else if out != "" {
			return StepOutput{StepID: step.ID, Status: "completed", Output: out}, nil
		}
	}

	if strings.TrimSpace(string(output)) == "" {
		recovered, source, out, ok := e.verifyCommitsAfterAutoCommit(taskID, wtPath, t)
		if ok {
			output = recovered
			finalSource = source
		} else if out != "" {
			return StepOutput{StepID: step.ID, Status: "completed", Output: out}, nil
		}
	}

	if strings.TrimSpace(string(output)) == "" {
		return e.verifyCommitsHandleEmptyOutput(taskID, step, wfExec, t, wtPath)
	}

	if finalSource == "" {
		finalSource = finalCommitSourceAgent
	}
	e.recordFinalCommitState(taskID, wfExec, wtPath, finalSource)
	e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyCommits, evidence.ProofDeterministicCheck,
		0, "git log <base>..HEAD --oneline", string(output))
	return StepOutput{StepID: step.ID, Status: "completed", Output: "commits verified"}, nil
}

func (e *Engine) parkVerifyCommitsForSiblingAgent(taskID string, wfExec *Execution) error {
	if e.agents == nil {
		return nil
	}
	exceptID := wfExec.LastAgentID()
	if !e.agents.HasOtherRunningAgentForTask(taskID, exceptID) {
		return nil
	}
	rearm := wfExec.LastAgentStepID()
	if rearm == "" {
		return nil
	}
	wfExec.CurrentStep = rearm
	wfExec.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}
	e.logger.Warn("workflow.verify-commits.parked",
		"task_id", taskID, "rearm_step", rearm, "except_agent", exceptID)
	return errStepParked
}

func (e *Engine) verifyCommitsGitErrorOutput(taskID, wtPath string, err error, prefix string) (string, bool) {
	if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
		e.logger.Warn("workflow.verify-commits.canceled", "task_id", taskID, "err", err)
		return "skipped: context canceled", true
	}
	diagnosis := diagnoseWorktreeState(e.ctx, wtPath)
	e.logger.Warn("workflow.verify-commits.git-error", "task_id", taskID, "worktree", wtPath, "err", err, "diagnosis", diagnosis)
	reason := prefix + err.Error()
	if diagnosis != "" {
		reason += " (" + diagnosis + ")"
	}
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
	}
	return "git error: flipped to human-required", true
}

func (e *Engine) verifyCommitsAfterRemoteAdopt(taskID, wtPath string, t TaskInfo) (output []byte, source, stepOutput string, ok bool) {
	_, adopted, err := e.adoptEquivalentRemoteCommit(taskID, wtPath, t)
	if err != nil {
		out, _ := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error before fallback reconcile: ")
		return nil, "", out, false
	}
	if !adopted {
		return nil, "", "", false
	}
	output, err = e.gitLogAheadOfBaseWithRetry(taskID, wtPath, t)
	if err != nil {
		out, _ := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error after remote reconcile: ")
		return nil, "", out, false
	}
	if strings.TrimSpace(string(output)) == "" {
		return output, "", "", false
	}
	return output, finalCommitSourceAgent, "", true
}

func (e *Engine) verifyCommitsAfterAutoCommit(taskID, wtPath string, t TaskInfo) (output []byte, source, stepOutput string, ok bool) {
	if !project.AutoCommitUncommitted(e.ctx, wtPath, "wip: auto-commit uncommitted implementation work\n\nverify_commits recovered work an agent finished without committing.") {
		return nil, "", "", false
	}
	e.logger.Warn("workflow.verify-commits.auto-committed", "task_id", taskID)
	if recovered, source, out, ok := e.verifyCommitsRecoveredRemoteAdopt(taskID, wtPath, t); ok || out != "" {
		if ok {
			return recovered, source, "", true
		}
		return nil, "", out, false
	}
	recovered, err := e.gitLogAheadOfBase(wtPath)
	if err != nil {
		out, _ := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error after auto-commit: ")
		return nil, "", out, false
	}
	if strings.TrimSpace(string(recovered)) == "" {
		return recovered, "", "", false
	}
	return recovered, finalCommitSourceFallback, "", true
}

func (e *Engine) verifyCommitsRecoveredRemoteAdopt(taskID, wtPath string, t TaskInfo) (output []byte, source, stepOutput string, ok bool) {
	// The agent may already be pushing an equivalent commit when we recover
	// uncommitted work locally. Give that agent-owned lineage the same bounded
	// retry window as the ref/object recovery path before we keep a duplicate
	// fallback commit.
	for attempt := 0; ; attempt++ {
		_, adopted, err := e.adoptEquivalentRemoteCommit(taskID, wtPath, t)
		if err != nil {
			out, _ := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error after auto-commit remote reconcile: ")
			return nil, "", out, false
		}
		if adopted {
			output, err = e.gitLogAheadOfBaseWithRetry(taskID, wtPath, t)
			if err != nil {
				out, _ := e.verifyCommitsGitErrorOutput(taskID, wtPath, err, "worktree git error after auto-commit remote reconcile: ")
				return nil, "", out, false
			}
			if strings.TrimSpace(string(output)) == "" {
				return output, "", "", false
			}
			return output, finalCommitSourceAgent, "", true
		}
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			return nil, "", "", false
		}
		if attempt >= len(verifyCommitsRetryBackoffs) {
			return nil, "", "", false
		}
		e.logger.Warn("workflow.verify-commits.remote-adopt-retry",
			"task_id", taskID,
			"worktree", wtPath,
			"attempt", attempt+1)
		verifyCommitsRetrySleep(verifyCommitsRetryBackoffs[attempt])
	}
}

func (e *Engine) verifyCommitsHandleEmptyOutput(taskID string, step *Step, wfExec *Execution, t TaskInfo, wtPath string) (StepOutput, error) {
	if wfExec != nil && wfExec.LastAgentStepFailed() {
		reason := "implementation agent failed before committing — no commits on branch"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
			e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.verify-commits.agent-failed", "task_id", taskID)
		e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyCommits, evidence.ProofDeterministicCheck, 1, "", reason)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "agent failed before commit: flipped to human-required"}, nil
	}
	if e.rearmNoCommitAuthorRun(taskID, wfExec, t) {
		return StepOutput{}, errStepParked
	}
	// branchMergedIntoBase(e.ctx, wtPath) used to gate a "done" verdict here,
	// on the theory that HEAD == base could mean the fix already landed via a
	// different branch. That check is unreachable-false in this exact spot:
	// the caller only reaches verifyCommitsHandleEmptyOutput when
	// gitLogAheadOfBase (baseRef..HEAD) already came back empty, and
	// "baseRef..HEAD has no commits" and "HEAD is an ancestor of baseRef" are
	// the same git fact stated two ways — merge-base --is-ancestor can never
	// disagree with an empty log range. So this branch was true unconditionally
	// on every call, silently marking done any task whose implementation agent
	// reported success but committed nothing. Confirmed live: two foundational
	// tasks under the durable-effects umbrella (issues #2658, #2659) landed
	// `done` with prNumber 0 and a branch byte-identical to origin/main —
	// zero code, zero diff. Always escalate instead, matching the sibling
	// agent-failed case above: a human must see and explain "the agent
	// reported success but committed nothing," never have it silently closed.
	reason := "no commits pushed to branch"
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyCommits, evidence.ProofDeterministicCheck, 1, "", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "no commits: flipped to human-required"}, nil
}

// markRunIncomplete downgrades a clean-exit code-author run that produced
// nothing. Best-effort: the retry it accompanies matters more than the
// bookkeeping, so a patch failure is logged rather than aborting the rearm.
func (e *Engine) markRunIncomplete(taskID, agentID string) {
	if agentID == "" {
		return
	}
	if err := e.tasks.MarkAgentRunIncomplete(taskID, agentID); err != nil {
		e.logger.Error("workflow.verify-commits.mark-incomplete", "task_id", taskID, "agent_id", agentID, "err", err)
	}
}

func (e *Engine) rearmNoCommitAuthorRun(taskID string, wfExec *Execution, t TaskInfo) bool {
	if wfExec == nil {
		return false
	}
	agentID := wfExec.LastAgentID()
	run, ok := agentRunInfoByID(t.AgentRuns, agentID)
	if !ok || !isCodeAuthorRun(run) {
		return false
	}
	// Correct the record before retrying. The completion handler derives
	// success from a clean exit, which is all it can see — but a code-author
	// run that produced no commit did not succeed, and every consumer of
	// Outcome (stats, evaluation, the human-review verdict gate, stale-run
	// recovery) otherwise believes the implementation landed.
	e.markRunIncomplete(taskID, agentID)
	stepID := wfExec.LastAgentStepID()
	if stepID == "" {
		return false
	}
	const counterKey = "step.verify_commits.no_commit_retry"
	if parseWorkflowInt(wfExec.Variables[counterKey]) >= 1 {
		return false
	}
	wfExec.SetVar(counterKey, "1")
	wfExec.SetVar(verifyReaskNoteVar, noCommitReaskNote(run))
	wfExec.SetVar(workflowRetryAfterVar, time.Now().UTC().Add(verifyChecksAutoFixBackoff).Format(time.RFC3339))
	wfExec.ClearStepRecords(stepID)
	wfExec.CurrentStep = stepID
	wfExec.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		e.logger.Error("workflow.verify-commits.no-commit-retry.workflow", "task_id", taskID, "err", err)
		return false
	}
	reason := "retrying implementation once after no commits were produced"
	if run.SubagentCallCount > 0 {
		reason = "retrying implementation once after a background subagent handoff produced no commits"
	}
	if err := e.tasks.UpdateTaskStatus(taskID, t.Status, reason); err != nil {
		e.logger.Error("workflow.verify-commits.no-commit-retry.status", "task_id", taskID, "err", err)
		return false
	}
	e.logger.Warn("workflow.verify-commits.no-commit-retry",
		"task_id", taskID, "step", stepID, "agent_id", agentID, "subagent_call_count", run.SubagentCallCount)
	return true
}

// noCommitReaskNote builds the retry-once prompt injection for a code-author
// run that ended without commits. A run that fanned out to subagent calls
// (SubagentCallCount > 0) gets a diagnosis specific to the background-handoff
// failure mode instead of the generic no-commit note: the one-shot CLI
// process tears down as soon as it emits its final response, which kills any
// delegated subagent work still in flight — so a run that hands off to a
// subagent and says it will "wait" for the result ends with nothing done, see
// backgroundTaskGuardrail for the mid-run equivalent of this warning.
func noCommitReaskNote(run AgentRunInfo) string {
	if run.SubagentCallCount > 0 {
		return "The previous implementation run delegated work to a background " +
			"subagent and ended before that work produced any commits — a one-shot " +
			"run's process exits as soon as it emits its final response, which kills " +
			"any subagent still working in the background. Do not delegate the " +
			"implementation to a background/forked subagent and end your turn waiting " +
			"on it. Make the required code changes directly yourself, then commit and " +
			"push them before finishing this run."
	}
	return "The previous implementation run completed without producing commits. " +
		"Make the required code changes, then commit and push them before finishing."
}

func (e *Engine) recordFinalCommitState(taskID string, wfExec *Execution, wtPath, source string) {
	if source == "" || wfExec == nil {
		return
	}
	agentID := wfExec.LastAgentID()
	if agentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	headSHA := revParseCommit(ctx, wtPath, "HEAD")
	if headSHA == "" {
		return
	}
	if err := e.tasks.RecordAgentRunFinalCommit(taskID, agentID, headSHA, source); err != nil {
		e.logger.Warn("workflow.verify-commits.record-final-commit", "task_id", taskID, "agent_id", agentID, "err", err)
	}
}

func (e *Engine) adoptEquivalentRemoteCommit(taskID, wtPath string, t TaskInfo) (source string, adopted bool, err error) {
	if !e.recoverVerifyCommitsRefs(taskID, wtPath, t) {
		return "", false, nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	branch := resolveVerifyCommitsBranch(ctx, wtPath, t)
	if !validVerifyCommitsBranch(branch) {
		return "", false, nil
	}
	remoteRef := "refs/remotes/origin/" + branch
	if revParseCommit(ctx, wtPath, remoteRef) == "" {
		return "", false, nil
	}

	worktreeTree, err := currentWorktreeTree(ctx, wtPath)
	if err != nil {
		return "", false, err
	}
	headTree := revParseTree(ctx, wtPath, "HEAD")
	remoteTree := revParseTree(ctx, wtPath, remoteRef)
	if headTree == "" || remoteTree == "" {
		return "", false, nil
	}

	if worktreeTree == remoteTree {
		baseRef := resolveOriginBase(ctx, wtPath)
		hasWork, err := remoteBranchCarriesWork(ctx, wtPath, baseRef, remoteRef, remoteTree)
		if err != nil {
			return "", false, err
		}
		if !hasWork {
			// The remote branch is byte-identical to base with nothing ahead
			// of it — adopting it would silently mark a no-op as done (an
			// implementation agent that hands off to a background subagent
			// and exits can leave a branch pushed but empty). Fall through
			// to the no-commits handling instead.
			return "", false, nil
		}
		if err := resetHardToRef(ctx, wtPath, remoteRef); err != nil {
			return "", false, err
		}
		e.logger.Info("workflow.verify-commits.remote-adopted", "task_id", taskID, "branch", branch, "reason", "equivalent_tree")
		return finalCommitSourceAgent, true, nil
	}
	if worktreeTree != headTree {
		return "", false, nil
	}
	if headTree == remoteTree {
		if err := resetHardToRef(ctx, wtPath, remoteRef); err != nil {
			return "", false, err
		}
		e.logger.Info("workflow.verify-commits.remote-adopted", "task_id", taskID, "branch", branch, "reason", "equivalent_commit")
		return finalCommitSourceAgent, true, nil
	}
	ff, err := isAncestorCommit(ctx, wtPath, "HEAD", remoteRef)
	if err != nil {
		return "", false, err
	}
	if !ff {
		return "", false, nil
	}
	if err := fastForwardToRef(ctx, wtPath, remoteRef); err != nil {
		return "", false, err
	}
	e.logger.Info("workflow.verify-commits.remote-adopted", "task_id", taskID, "branch", branch, "reason", "fast_forward")
	return finalCommitSourceAgent, true, nil
}

// remoteBranchCarriesWork reports whether remoteRef represents real work
// relative to baseRef: either its tree differs from base's, or it has
// commits base does not (a merge/revert lineage that nets out to base's
// tree but still carries history). Guards equivalent-tree remote adoption so
// a branch pushed byte-identical to base with zero commits ahead is never
// mistaken for completed work.
func remoteBranchCarriesWork(ctx context.Context, wtPath, baseRef, remoteRef, remoteTree string) (bool, error) {
	baseTree := revParseTree(ctx, wtPath, baseRef)
	if baseTree == "" || baseTree != remoteTree {
		return true, nil
	}
	ahead, err := gitCombinedOutput(ctx, wtPath, "log", baseRef+".."+remoteRef, "--oneline")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(ahead)) != "", nil
}

func (e *Engine) gitLogAheadOfBaseWithRetry(taskID, wtPath string, t TaskInfo) ([]byte, error) {
	output, err := e.gitLogAheadOfBase(wtPath)
	if err != nil && shouldRetryVerifyCommitsGitError(err, output) && e.recoverVerifyCommitsRefs(taskID, wtPath, t) {
		output, err = e.gitLogAheadOfBase(wtPath)
	}
	for attempt := 0; err != nil && shouldRetryVerifyCommitsGitError(err, output); attempt++ {
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			break
		}
		if attempt >= len(verifyCommitsRetryBackoffs) {
			break
		}
		e.logger.Warn("workflow.verify-commits.retry",
			"task_id", taskID,
			"worktree", wtPath,
			"attempt", attempt+1,
			"err", err)
		verifyCommitsRetrySleep(verifyCommitsRetryBackoffs[attempt])
		output, err = e.gitLogAheadOfBase(wtPath)
	}
	return output, err
}

// branchMergedIntoBase reports whether HEAD is reachable from the resolved
// origin base ref (i.e. HEAD is an ancestor of, or equal to, base). Used by
// execVerifyCommits to distinguish "fix already merged into origin" from
// "agent did nothing".
//
// `git log origin/main..HEAD` empty ⟺ HEAD is reachable from origin/main, so
// any time we land in the "no commits ahead" branch the answer here is true
// in the common case. We still gate on an explicit `merge-base --is-ancestor`
// check so an unrelated history (orphan branch, force-pushed elsewhere) does
// not silently get marked done.
func branchMergedIntoBase(parentCtx context.Context, wtPath string) bool {
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()
	baseRef := resolveOriginBase(ctx, wtPath)
	return gitIsAncestor(ctx, wtPath, "HEAD", baseRef)
}

func (e *Engine) gitLogAheadOfBase(wtPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	baseRef := resolveOriginBase(ctx, wtPath)
	return gitCombinedOutput(ctx, wtPath, "log", baseRef+"..HEAD", "--oneline")
}

func shouldRetryVerifyCommitsGitError(err error, output []byte) bool {
	if err == nil {
		return false
	}
	return errclass.IsBadRef(err.Error() + "\n" + string(output))
}

func (e *Engine) recoverVerifyCommitsRefs(taskID, wtPath string, t TaskInfo) bool {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	branch := resolveVerifyCommitsBranch(ctx, wtPath, t)
	if !validVerifyCommitsBranch(branch) {
		e.logger.Warn("workflow.verify-commits.ref-recovery.skip", "task_id", taskID, "branch", branch)
		return false
	}

	baseRef := resolveOriginBase(ctx, wtPath)
	negotiationTips := verifyCommitsNegotiationTips(ctx, wtPath, baseRef)
	barePath, bareErr := project.CommonDir(ctx, wtPath)
	if bareErr != nil {
		barePath = ""
	}
	for _, ref := range pruneBrokenVerifyCommitRefs(ctx, wtPath, barePath, branch) {
		e.logger.Warn("workflow.verify-commits.ref-recovery.pruned-broken-ref", "task_id", taskID, "ref", ref)
	}
	refspecs := []string{"+" + "refs/heads/" + branch + ":refs/remotes/origin/" + branch}
	if baseBranch, ok := strings.CutPrefix(baseRef, "origin/"); ok && baseBranch != "" && baseBranch != "HEAD" {
		refspecs = append(refspecs, "+"+"refs/heads/"+baseBranch+":refs/remotes/origin/"+baseBranch)
	}

	_ = gitDo(ctx, wtPath, "update-ref", "-d", "refs/remotes/origin/"+branch)
	if baseBranch, ok := strings.CutPrefix(baseRef, "origin/"); ok && baseBranch != "" && baseBranch != "HEAD" {
		_ = gitDo(ctx, wtPath, "update-ref", "-d", "refs/remotes/origin/"+baseBranch)
	}

	args := []string{"fetch", "--no-tags", "--no-recurse-submodules"}
	for _, tip := range negotiationTips {
		args = append(args, "--negotiation-tip="+tip)
	}
	args = append(args, "origin")
	args = append(args, refspecs...)
	if _, err := gitCombinedOutput(ctx, wtPath, args...); err != nil {
		e.logger.Warn("workflow.verify-commits.ref-recovery.failed", "task_id", taskID, "branch", branch, "err", err)
		return false
	}
	e.logger.Warn("workflow.verify-commits.ref-recovery.fetched", "task_id", taskID, "branch", branch, "base", baseRef)
	return true
}

func resolveVerifyCommitsBranch(ctx context.Context, wtPath string, t TaskInfo) string {
	return strings.TrimSpace(t.Branch)
}

func validVerifyCommitsBranch(branch string) bool {
	return branch != "" && !strings.HasPrefix(branch, "-") && !strings.Contains(branch, "..") && !strings.Contains(branch, " ")
}

func verifyCommitsNegotiationTips(ctx context.Context, wtPath, baseRef string) []string {
	if baseRef == "" {
		return nil
	}
	if sha := revParseCommit(ctx, wtPath, baseRef); sha != "" {
		return []string{sha}
	}
	return nil
}

func pruneBrokenVerifyCommitRefs(ctx context.Context, wtPath, barePath, taskBranch string) []string {
	out, err := gitStdout(ctx, wtPath, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil
	}

	keepLocalBranch := "refs/heads/" + taskBranch
	var pruned []string
	for line := range strings.SplitSeq(out, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || ref == keepLocalBranch {
			continue
		}
		if gitOK(ctx, wtPath, "cat-file", "-e", ref+"^{object}") {
			continue
		}
		if barePath != "" {
			sha, _ := gitStdout(ctx, wtPath, "rev-parse", ref)
			project.QuarantineRef(barePath, ref, sha)
		}
		if gitDo(ctx, wtPath, "update-ref", "-d", ref) == nil {
			pruned = append(pruned, ref)
		}
	}
	return pruned
}

func revParseCommit(ctx context.Context, wtPath, ref string) string {
	sha, _ := gitRevParseVerify(ctx, wtPath, ref+"^{commit}")
	return sha
}

func revParseTree(ctx context.Context, wtPath, ref string) string {
	sha, _ := gitRevParseVerify(ctx, wtPath, ref+"^{tree}")
	return sha
}

func currentWorktreeTree(ctx context.Context, wtPath string) (string, error) {
	tmpIndex, err := os.CreateTemp("", "sybra-verify-index-*")
	if err != nil {
		return "", fmt.Errorf("create temp index: %w", err)
	}
	path := tmpIndex.Name()
	_ = tmpIndex.Close()
	defer func() { _ = os.Remove(path) }()

	opts := gitexec.Options{Dir: wtPath, ExtraEnv: []string{"GIT_INDEX_FILE=" + path}}
	if _, err := gitexec.CombinedOutput(ctx, opts, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := gitexec.CombinedOutput(ctx, opts, "add", "-A"); err != nil {
		return "", err
	}
	out, err := gitexec.CombinedOutput(ctx, opts, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func resetHardToRef(ctx context.Context, wtPath, ref string) error {
	_, err := gitCombinedOutput(ctx, wtPath, "reset", "--hard", ref)
	return err
}

func fastForwardToRef(ctx context.Context, wtPath, ref string) error {
	_, err := gitCombinedOutput(ctx, wtPath, "merge", "--ff-only", ref)
	return err
}

func isAncestorCommit(ctx context.Context, wtPath, ancestor, ref string) (bool, error) {
	err := gitexec.RunQuiet(ctx, gitexec.Options{Dir: wtPath}, "merge-base", "--is-ancestor", ancestor, ref)
	if err == nil {
		return true, nil
	}
	if code, ok := gitexec.ExitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, err
}

// diagnoseWorktreeState produces a short human-readable hint about why a
// worktree's `git log` failed. Returns "" when nothing concrete is found.
// Used to enrich the human-required status_reason so triage doesn't have
// to grep agent logs to figure out whether the worktree was missing,
// dirty, or just had a stale lock.
func diagnoseWorktreeState(parentCtx context.Context, wtPath string) string {
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()

	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{Dir: wtPath}, "status", "--porcelain")
	if err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		first = strings.TrimSpace(first)
		if first == "" {
			return "git status failed: " + err.Error()
		}
		return "git status: " + first
	}
	if dirty := strings.TrimSpace(string(out)); dirty != "" {
		entries := strings.Count(dirty, "\n") + 1
		return fmt.Sprintf("dirty worktree (%d uncommitted entries)", entries)
	}
	return "clean tree, no commits ahead"
}
