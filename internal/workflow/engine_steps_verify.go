package workflow

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/github"
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
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", "PR does not close linked issue and auto-fix failed: "+editErr.Error()); statusErr != nil {
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
	switch sk := step.Config.Sidecar; {
	case sk == "plan_critique":
		content = t.PlanCritique
		label = "plan critique"
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
		return StepOutput{}, fmt.Errorf("require_sidecar: unknown sidecar %q (want plan|plan_critique|code_review|plan_draft.<name>)", sk)
	}
	if strings.TrimSpace(content) == "" {
		reason := label + " missing — upstream agent step completed without writing its sidecar"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.require-sidecar.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.require-sidecar.missing", "task_id", taskID, "sidecar", step.Config.Sidecar)
		return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: label + " present"}, nil
}

// resolveOriginBase returns the remote ref to use as the base for commit
// range comparisons. It checks for origin/HEAD (set when the remote HEAD
// symbolic ref is configured), then falls back to probing master and main.
// Returns "origin/main" if nothing resolves.
func resolveOriginBase(ctx context.Context, wtPath string) string {
	for _, candidate := range []string{"origin/HEAD", "origin/master", "origin/main"} {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", candidate)
		cmd.Dir = wtPath
		if cmd.Run() == nil {
			return candidate
		}
	}
	return "origin/main"
}

func (e *Engine) execVerifyCommits(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	output, err := e.gitLogAheadOfBase(wtPath)
	if err != nil && !errors.Is(e.ctx.Err(), context.Canceled) && !errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
		verifyCommitsRetrySleep(verifyCommitsRetryBackoff)
		output, err = e.gitLogAheadOfBase(wtPath)
	}
	if err != nil {
		// Context cancellation indicates engine shutdown, not a worktree
		// problem — leave task status alone so it resumes on next boot.
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			e.logger.Warn("workflow.verify-commits.canceled", "task_id", taskID, "err", err)
			return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: context canceled"}, nil
		}
		diagnosis := diagnoseWorktreeState(e.ctx, wtPath)
		e.logger.Warn("workflow.verify-commits.git-error", "task_id", taskID, "worktree", wtPath, "err", err, "diagnosis", diagnosis)
		reason := "worktree git error: " + err.Error()
		if diagnosis != "" {
			reason += " (" + diagnosis + ")"
		}
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "git error: flipped to human-required"}, nil
	}

	if strings.TrimSpace(string(output)) == "" {
		// A crashed implementation agent (e.g. API error mid-run) leaves a fresh
		// worktree branch sitting exactly on the base tip — git-indistinguishable
		// from "work already merged via another branch". Marking such a task done
		// silently discards the uncommitted work. Disambiguate with the upstream
		// agent step's outcome: a failed implement step means the branch is empty
		// because the agent died, not because the fix already shipped. Route to
		// human-required so the run is surfaced, not closed.
		if wfExec.LastAgentStepFailed() {
			reason := "implementation agent failed before committing — no commits on branch"
			if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
				e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
			}
			e.logger.Warn("workflow.verify-commits.agent-failed", "task_id", taskID)
			return StepOutput{StepID: step.ID, Status: "completed", Output: "agent failed before commit: flipped to human-required"}, nil
		}
		// Disambiguate "no commits ahead" between two cases:
		//   (a) HEAD == base ref tip → branch is identical to base. Implementation
		//       was already on origin (e.g. merged via a different branch before
		//       this task ran). Re-running implement would loop forever because
		//       there is nothing left to do. Mark done so the auto-restart in
		//       svc_tasks.UpdateTask doesn't bounce the task back to in-progress.
		//   (b) HEAD != base → branch diverged but has zero fast-forward commits.
		//       Genuine "agent did nothing"; flip to human-required as before.
		if branchMergedIntoBase(e.ctx, wtPath) {
			reason := "branch already merged into base — implementation already on origin"
			if statusErr := e.tasks.UpdateTaskStatus(taskID, "done", reason); statusErr != nil {
				e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
			}
			e.logger.Info("workflow.verify-commits.branch-merged", "task_id", taskID)
			return StepOutput{StepID: step.ID, Status: "completed", Output: "branch merged into base: marked done"}, nil
		}
		reason := "no commits pushed to branch"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "no commits: flipped to human-required"}, nil
	}

	return StepOutput{StepID: step.ID, Status: "completed", Output: "commits verified"}, nil
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
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", "HEAD", baseRef)
	cmd.Dir = wtPath
	return cmd.Run() == nil
}

func (e *Engine) gitLogAheadOfBase(wtPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	baseRef := resolveOriginBase(ctx, wtPath)
	cmd := exec.CommandContext(ctx, "git", "log", baseRef+"..HEAD", "--oneline")
	cmd.Dir = wtPath
	return cmd.Output()
}

// diagnoseWorktreeState produces a short human-readable hint about why a
// worktree's `git log` failed. Returns "" when nothing concrete is found.
// Used to enrich the human-required status_reason so triage doesn't have
// to grep agent logs to figure out whether the worktree was missing,
// dirty, or just had a stale lock.
func diagnoseWorktreeState(parentCtx context.Context, wtPath string) string {
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
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
