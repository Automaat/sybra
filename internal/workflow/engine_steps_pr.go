package workflow

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/project"
)

// execPushBranch deterministically pushes the task's worktree branch to its
// existing PR (task.pr_number already set — reached only via the
// maybe_create_pr retry path in simple-task-pr). Replaces the push-branch
// agent role: no LLM/agent involved, only git plumbing.
func (e *Engine) execPushBranch(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	wtPath, branch, out, done := e.prWorktreeAndBranch(taskID, step, t)
	if done {
		return out, nil
	}

	if out, err, ok := e.pushTaskBranch(taskID, step, wfExec, t, wtPath, branch); !ok {
		return out, err
	}

	// Best-effort verification that the PR head now matches local HEAD,
	// mirroring the retired push-branch agent's SHA check. A mismatch
	// (propagation lag, or a race with another pusher) is logged, not fatal —
	// link_pr_and_review still finds task.pr_number regardless, and a stale
	// head would surface as a failing PR check rather than a silent miss.
	if e.prHeads != nil && t.PRNumber > 0 && t.ProjectID != "" {
		e.verifyPushedHead(taskID, wtPath, t)
	}

	return stepDone(step, fmt.Sprintf("pushed %s", branch))
}

// execCreatePR deterministically pushes the task's worktree branch and opens
// a GitHub PR for it, drafting the title/body via a single cheap LLM job
// (internal/prcontent). Replaces the create-pr agent role.
func (e *Engine) execCreatePR(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if t.ProjectID == "" {
		return e.humanRequiredPR(taskID, step, "task has no project — cannot open a PR")
	}

	wtPath, branch, out, done := e.prWorktreeAndBranch(taskID, step, t)
	if done {
		return out, nil
	}

	headArg, err := func() (string, error) {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		return project.HeadArg(ctx, wtPath, branch)
	}()
	if err != nil {
		return e.humanRequiredPR(taskID, step, "could not resolve pr head: "+err.Error())
	}

	// Idempotency guard: a prior run may have created the PR without
	// persisting pr_number (crash/restart between `gh pr create` and
	// UpdateTaskPR). Mirrors the retired create-pr agent's own existence
	// check before creating a duplicate. Uses headArg (not the bare branch)
	// so a fork-hosted branch, which GitHub only matches as "owner:branch",
	// is found too.
	if existing, ok := e.findExistingPRForBranch(t.ProjectID, headArg); ok {
		if err := e.tasks.UpdateTaskPR(taskID, existing); err != nil {
			return StepOutput{}, fmt.Errorf("create_pr: link existing pr: %w", err)
		}
		return stepDone(step, fmt.Sprintf("pr #%d already exists for branch", existing))
	}

	if out, err, ok := e.pushTaskBranch(taskID, step, wfExec, t, wtPath, branch); !ok {
		return out, err
	}

	title, body := e.generatePRContent(taskID, wtPath, t)

	if e.prCreator == nil {
		return e.humanRequiredPR(taskID, step, "no PR creator configured")
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	number, headSHA, createErr := e.prCreator.CreatePR(ctx, wtPath, PRCreateRequest{
		Repo:  t.ProjectID,
		Head:  headArg,
		Draft: t.ProjectType != "pet",
		Title: title,
		Body:  body,
	})
	if createErr != nil {
		return e.classifyPRGitError(taskID, step, wfExec, t, createErr, "create_pr")
	}

	if err := e.tasks.UpdateTaskPR(taskID, number); err != nil {
		return StepOutput{}, fmt.Errorf("create_pr: link pr: %w", err)
	}
	if localSHA, lErr := project.CurrentCommit(ctx, wtPath); lErr == nil && headSHA != "" && localSHA != headSHA {
		e.logger.Warn("workflow.create-pr.head-mismatch", "task_id", taskID, "pr", number, "local", localSHA, "remote", headSHA)
	}
	e.logger.Info("workflow.create-pr.created", "task_id", taskID, "pr", number)
	return stepDone(step, fmt.Sprintf("created pr #%d", number))
}

// prWorktreeAndBranch resolves the on-disk worktree and branch used by
// push_branch/create_pr. Both are reached only from the ready-pr stage of
// simple-task-pr, after implementation/testing already produced a worktree —
// a missing WorktreeGetter or worktree is therefore an unrecoverable setup
// problem, not a transient one, so it flips straight to human-required.
func (e *Engine) prWorktreeAndBranch(taskID string, step *Step, t TaskInfo) (wtPath, branch string, out StepOutput, done bool) {
	if e.worktrees == nil {
		out, _ = e.humanRequiredPR(taskID, step, "no worktree getter configured")
		return "", "", out, true
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		out, _ = e.humanRequiredPR(taskID, step, "no worktree found for task")
		return "", "", out, true
	}
	branch = t.Branch
	if branch == "" {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		resolved, err := project.CurrentBranch(ctx, wtPath)
		if err != nil || resolved == "" {
			reason := "could not determine branch"
			if err != nil {
				reason += ": " + err.Error()
			}
			out, _ = e.humanRequiredPR(taskID, step, reason)
			return "", "", out, true
		}
		branch = resolved
	}
	return wtPath, branch, StepOutput{}, false
}

// humanRequiredPR flips the task to human-required with reason and returns a
// completed StepOutput carrying the same reason, matching the pattern used
// throughout the other PR-tail steps (e.g. execRequireSidecar).
func (e *Engine) humanRequiredPR(taskID string, step *Step, reason string) (StepOutput, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("%s: set human-required: %w", step.ID, err)
	}
	e.logger.Warn("workflow.pr-tail.human-required", "task_id", taskID, "step", step.ID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

// pushTaskBranch pushes wtPath's branch via project.PushSync and classifies
// the outcome for retry/escalation. ok is false when the caller must return
// (out, err) immediately — either the push failed permanently (human-required)
// or it was parked for a bounded retry (errStepParked).
func (e *Engine) pushTaskBranch(taskID string, step *Step, wfExec *Execution, t TaskInfo, wtPath, branch string) (out StepOutput, err error, ok bool) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	pushErr := project.PushSync(ctx, wtPath, branch)
	if pushErr == nil {
		return StepOutput{}, nil, true
	}

	// Never force-push: a genuine divergence needs a human (or a dedicated
	// conflict-fix flow) to reconcile, exactly like the retired agent
	// prompts instructed. Same for a missing local branch ref — both are
	// unrecoverable by retrying the push.
	if errors.Is(pushErr, project.ErrDivergedNeedsResolve) {
		// Try the same autonomous conflict-fix recovery agentorch dispatches
		// for worktree-prep rebase conflicts before giving up to a human — a
		// divergence here is often self-inflicted (a reused worktree rebased
		// onto a newer base after an earlier merge-based push), and
		// RecoverStaleBranchConflict resolves it the same way regardless of
		// origin. It cancels this task's active workflow and starts
		// branch-conflict-fix in its place (capturing resume state to
		// re-enter this step on success), so the caller must treat the
		// current workflow execution as already handled — errStepParked
		// mirrors parkStepForRetry's contract for exactly that case.
		if e.conflictRecovery != nil {
			e.logger.Info("workflow.pr-tail.branch-conflict.recover",
				"task_id", taskID, "step", step.ID)
			if e.tryConflictRecovery(taskID) {
				return StepOutput{}, errStepParked, false
			}
			// Recovery ran inline (no marker held) and declined — fall through
			// to the human escalation below.
		}
		out, err = e.humanRequiredPR(taskID, step, "branch diverged from remote — needs manual conflict resolution (never force-pushed): "+pushErr.Error())
		return out, err, false
	}
	if errors.Is(pushErr, project.ErrBranchMissing) {
		out, err = e.humanRequiredPR(taskID, step, "local branch ref missing: "+pushErr.Error())
		return out, err, false
	}

	out, err = e.classifyPRGitError(taskID, step, wfExec, t, pushErr, "git push")
	return out, err, false
}

// tryConflictRecovery attempts autonomous branch-conflict recovery for a
// diverged push and reports whether the step must park.
//
// The callback (review.Handler.RecoverStaleBranchConflict) cancels this task's
// active workflow and launches branch-conflict-fix — a re-entry into
// StartWorkflow*/DispatchEvent. When push_branch/create_pr runs inside one of
// those calls (DispatchEvent → startWorkflowLocked), the per-task starting
// marker is still held, so invoking recovery
// now would hit ErrWorkflowAlreadyActive and silently no-op — the reentrancy
// trap that made this whole path dead on arrival. Detect that case by the held
// marker and queue the recovery instead; drainPendingConflictRecovery runs it
// once the outer call releases the marker (returns true → park now). When no
// marker is held (the AdvanceStep tail, or a direct call), recovery is safe to
// run inline and its own verdict is returned.
func (e *Engine) tryConflictRecovery(taskID string) bool {
	return e.tryConflictRecoveryWithFallback(taskID, func() {
		e.escalatePendingConflictRecovery(taskID)
	})
}

func (e *Engine) tryConflictRecoveryWithFallback(taskID string, onDecline func()) bool {
	e.mu.Lock()
	_, starting := e.starting[taskID]
	_, dispatching := e.dispatching[taskID]
	if starting || dispatching {
		if e.pendingRecovery == nil {
			e.pendingRecovery = make(map[string]pendingRecovery)
		}
		e.pendingRecovery[taskID] = pendingRecovery{onDecline: onDecline}
		e.mu.Unlock()
		return true
	}
	e.mu.Unlock()
	return e.conflictRecovery(taskID)
}

// TryConflictRecovery is the exported entry point for callers outside this
// package that invoke the same review.Handler.RecoverStaleBranchConflict
// callback as push_branch/create_pr: agentorch's worktree-prep rebase-failure
// path (MarkRebaseBlockedWithRecoveryResult) and the fix-review
// push-divergence handler. A rebase conflict discovered while this task's own
// StartWorkflow call is still executing (e.g. execRunAgent's worktree prep,
// deep inside the same synchronous stack that holds the starting marker)
// needs the identical queue-instead-of-reenter treatment tryConflictRecovery
// already gives push_branch/create_pr — without it, the recovery callback
// races straight into ErrWorkflowAlreadyActive and gives up, stranding the
// task on a raw ClassifyAgentStartError escalation instead of the autonomous
// fix. Nil-receiver-safe (mirrors RecoverStaleBranchConflict's own guard) so
// wiring this in ahead of a possibly-nil workflowEngine during degraded init
// is safe. Returns false when the receiver or the recovery callback is nil.
func (e *Engine) TryConflictRecovery(taskID string) bool {
	if e == nil || e.conflictRecovery == nil {
		return false
	}
	return e.tryConflictRecovery(taskID)
}

// QueueConflictRecoveryRetry defers a conflict-recovery retry until this
// task's starting marker next releases, for a caller whose own launch of the
// recovery workflow (e.g. dispatchBranchConflictRecovery's
// StartWorkflowWithVars call) hit ErrWorkflowAlreadyActive despite
// TryConflictRecovery's entry check having found no marker held. The marker
// can be grabbed by a concurrent StartWorkflow call sometime during the
// caller's own multi-second worktree-prep work — a TOCTOU window
// TryConflictRecovery's up-front check alone cannot close. drainPendingConflictRecovery
// re-invokes the full recovery callback once whichever call currently holds
// the marker releases it.
func (e *Engine) QueueConflictRecoveryRetry(taskID string) {
	e.mu.Lock()
	if e.pendingRecovery == nil {
		e.pendingRecovery = make(map[string]pendingRecovery)
	}
	e.pendingRecovery[taskID] = pendingRecovery{onDecline: func() {
		e.escalatePendingConflictRecovery(taskID)
	}}
	e.mu.Unlock()
}

// drainPendingConflictRecovery runs a branch-conflict recovery that
// tryConflictRecovery deferred because a per-task marker was held when the
// diverged push was detected. It MUST be called only after the caller has
// released its starting marker (alongside fireComplete), so the callback's
// launch is not rejected as re-entrant. No-op when nothing was
// queued for the task. When recovery is unavailable or declines, the task is
// escalated to human-required and its parked workflow terminated — the same
// terminal outcome the inline divergence path produces.
func (e *Engine) drainPendingConflictRecovery(taskID string) {
	e.mu.Lock()
	pending, ok := e.pendingRecovery[taskID]
	if ok {
		delete(e.pendingRecovery, taskID)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	if e.conflictRecovery != nil && e.conflictRecovery(taskID) {
		return // recovery cancelled this workflow and dispatched branch-conflict-fix
	}
	if pending.onDecline != nil {
		pending.onDecline()
		return
	}
	e.escalatePendingConflictRecovery(taskID)
}

func (e *Engine) escalatePendingConflictRecovery(taskID string) {
	reason := "branch diverged from remote — needs manual conflict resolution (never force-pushed)"
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		e.logger.Error("workflow.pr-tail.conflict-recovery.escalate", "task_id", taskID, "err", err)
	}
	if _, err := e.CancelWorkflow(taskID, reason); err != nil {
		e.logger.Error("workflow.pr-tail.conflict-recovery.cancel", "task_id", taskID, "err", err)
	}
}

// classifyPRGitError inspects a push/create failure and either parks the
// step for a bounded retry (rate limit, transient network blip, or a bounded
// number of auth retries) or escalates to human-required. Reuses the same
// classification helpers/vars execEvaluate uses for its PR-creation retry
// path (engine_steps_link.go), since push_branch/create_pr now own this
// failure handling directly instead of falling through to evaluate.
func (e *Engine) classifyPRGitError(taskID string, step *Step, wfExec *Execution, t TaskInfo, err error, phase string) (StepOutput, error) {
	msg := err.Error()
	switch {
	case looksLikeGitHubRateLimit(msg):
		return e.parkStepForRetry(taskID, wfExec, t, step.ID, prCreateRetryStatusReason, "workflow.pr-tail.rate-limit", "phase", phase)
	case looksLikeTransientGitHub(msg):
		return e.parkStepForRetry(taskID, wfExec, t, step.ID, prCreateTransientStatusReason, "workflow.pr-tail.transient", "phase", phase)
	case looksLikeAuthFailure(msg):
		attempts := parseWorkflowInt(wfExec.Variables[prCreateAuthAttemptsVar])
		if attempts < maxPRCreateAuthRetries {
			wfExec.SetVar(prCreateAuthAttemptsVar, strconv.Itoa(attempts+1))
			return e.parkStepForRetry(taskID, wfExec, t, step.ID, prCreateAuthRetryReason, "workflow.pr-tail.auth-retry", "attempt", attempts+1, "max", maxPRCreateAuthRetries)
		}
		return e.humanRequiredPR(taskID, step, fmt.Sprintf("%s failing due to invalid or expired GitHub credentials after %d retries: %s", phase, attempts, msg))
	default:
		return e.humanRequiredPR(taskID, step, phase+" failed: "+msg)
	}
}

// findExistingPRForBranch checks for a PR already open on branch, mirroring
// the gh-list idempotency guard the retired create-pr agent ran before
// creating a new PR. Delegates to the injected PRFinder (github.FindPRForBranch
// under ghGate/ghEnv) so the lookup shares the same rate-limit pacing and
// GitHub identity as the create call it guards. Best-effort: a nil finder or a
// lookup failure is treated as "no PR found" so create_pr proceeds rather than
// getting stuck.
func (e *Engine) findExistingPRForBranch(repo, branch string) (number int, ok bool) {
	if e.prFinder == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	num, found, err := e.prFinder.FindPRForBranch(ctx, repo, branch)
	if err != nil {
		e.logger.Warn("workflow.create-pr.find-existing", "repo", repo, "head", branch, "err", err)
		return 0, false
	}
	return num, found
}

// verifyPushedHead best-effort verifies the PR head now matches local HEAD
// after a push, retrying briefly to absorb GitHub's propagation lag. Only
// logs on mismatch — never blocks the workflow.
func (e *Engine) verifyPushedHead(taskID, wtPath string, t TaskInfo) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	localSHA, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		e.logger.Warn("workflow.push-branch.local-sha", "task_id", taskID, "err", err)
		return
	}
	var remoteSHA string
	for attempt := 0; attempt <= len(prVerifyBackoffs); attempt++ {
		if attempt > 0 {
			prVerifySleep(prVerifyBackoffs[attempt-1])
		}
		shaCtx, shaCancel := context.WithTimeout(e.ctx, shellTimeout)
		var shaErr error
		remoteSHA, shaErr = e.prHeads.FetchPRHeadSHA(shaCtx, t.ProjectID, t.PRNumber)
		shaCancel()
		if shaErr == nil && remoteSHA == localSHA {
			return
		}
	}
	e.logger.Warn("workflow.push-branch.head-mismatch", "task_id", taskID, "pr", t.PRNumber, "local", localSHA, "remote", remoteSHA)
}

// generatePRContent drafts a PR title/body via e.prContentGen, falling back
// to a minimal templated title/body (task title verbatim, task body under a
// Motivation heading) when no generator is wired or the generation failed —
// so create_pr always has something valid to hand `gh pr create`.
func (e *Engine) generatePRContent(taskID, wtPath string, t TaskInfo) (title, body string) {
	fallbackTitle := t.Title
	fallbackBody := "## Motivation\n\n" + strings.TrimSpace(t.Body) + "\n\n## Implementation information\n\nSee commit history for " + t.Title + "."
	if e.prContentGen == nil {
		return fallbackTitle, fallbackBody
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	subjects := commitSubjects(ctx, wtPath)
	genTitle, genBody, genErr := e.prContentGen.GeneratePRContent(ctx, t.Title, t.Body, subjects)
	if genErr != nil || strings.TrimSpace(genTitle) == "" || strings.TrimSpace(genBody) == "" {
		e.logger.Warn("workflow.create-pr.content-fallback", "task_id", taskID, "err", genErr)
		return fallbackTitle, fallbackBody
	}
	return genTitle, genBody
}

// commitSubjects returns the one-line subjects of every commit on wtPath's
// current branch that is not yet on the resolved origin base, oldest first.
// Best-effort: a git failure yields an empty slice, not an error — the PR
// content prompt just has less context to work with.
func commitSubjects(ctx context.Context, wtPath string) []string {
	base := resolveOriginBase(ctx, wtPath)
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%s", "--reverse", base+"..HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var subjects []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects
}
