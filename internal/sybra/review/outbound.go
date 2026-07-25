package review

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/intervention"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const linkedPRDriftWindow = 10 * time.Minute

// reconciledLatchTag marks a task whose human-required blocker was already
// auto-reconciled by reconcileHumanRequiredBlockers, so a later poll cycle
// never re-matches and re-reconciles the same stale exhaustion reason after
// the task has already been put back into motion once.
const reconciledLatchTag = "monitor:reconciled"

// reconcilePRPhases recomputes the lifecycle phase of every outbound own-PR
// task (status in-review/ready-review, not tag `review`) from the live
// monitored PRs and persists any delta. The phase is a pure overlay on the In
// Review column — it never changes task.Status.
func (r *Handler) reconcilePRPhases(ctx context.Context, tasks []task.Task, monitoredPRs []github.PullRequest) {
	byNumber, byBranch := indexMonitoredPRs(monitoredPRs)

	for i := range tasks {
		t := &tasks[i]
		if repaired := r.reactivateLinkedOwnPR(t, matchingPR(t, byNumber, byBranch) != nil); repaired != nil {
			tasks[i] = *repaired
			t = repaired
		}
		if !ownPRColumnTask(t) {
			// Clear a stale phase left over from when the task was in review
			// (e.g. it was moved back to in-progress or its PR closed) so the
			// glyph only ever shows in the In Review column.
			if t.PRPhase != "" {
				r.applyPRPhase(t, "")
			}
			continue
		}

		pr := matchingPR(t, byNumber, byBranch)
		if pr == nil {
			// PR not in the current summary yet (just opened / cache miss).
			// Leave the existing phase untouched until it appears.
			continue
		}

		r.applyPRPhase(t, computePRPhase(prSignals{
			AgentRunning:     r.hasBlockingAgentForTask(ctx, t.ID),
			IsDraft:          pr.IsDraft,
			CIStatus:         pr.CIStatus,
			HasPendingChecks: pr.HasPendingChecks,
			Mergeable:        pr.Mergeable,
			ReviewDecision:   pr.ReviewDecision,
			UnresolvedCount:  pr.UnresolvedCount,
			ActionableCount:  pr.ActionableCount,
		}))
	}
}

// cancelSettledImplementationWorkflows terminates a stale simple-task-implement
// workflow once the task's branch already has an open, green PR. Without this,
// ResumeStalled keeps re-dispatching the implement step even though ownership
// of the work already moved to the PR monitor / in-review lane.
//
// Intentionally scoped to the implement step itself. A workflow parked on later
// deterministic gates (verify_checks, etc.) still owns meaningful work and must
// not be short-circuited just because an older PR snapshot looks green.
func (r *Handler) cancelSettledImplementationWorkflows(ctx context.Context, tasks []task.Task, monitoredPRs []github.PullRequest) {
	if r.WorkflowEngine == nil {
		return
	}
	byNumber, byBranch := indexMonitoredPRs(monitoredPRs)

	for i := range tasks {
		t := &tasks[i]
		if !staleImplementationWorkflowEligible(t) {
			continue
		}
		if r.hasRunningAgentForTask(ctx, t.ID) {
			continue
		}
		pr := matchingPR(t, byNumber, byBranch)
		if pr == nil || !settledOwnPR(*pr) {
			continue
		}

		step, err := r.WorkflowEngine.CancelWorkflow(t.ID, "pr-monitor: implementation superseded by settled PR")
		if err != nil {
			r.logger.Error("pr-monitor.cancel-implement", "task_id", t.ID, "pr", pr.Number, "err", err)
			continue
		}

		upd := task.Update{
			Status:       task.Ptr(task.StatusInReview),
			StatusReason: task.Ptr(""),
		}
		if t.PRNumber == 0 && pr.Number > 0 {
			upd.PRNumber = task.Ptr(pr.Number)
		}
		updated, err := r.tasks.Update(t.ID, upd)
		if err != nil {
			r.logger.Error("pr-monitor.cancel-implement.status", "task_id", t.ID, "pr", pr.Number, "err", err)
			continue
		}
		tasks[i] = updated
		r.logger.Info("pr-monitor.cancel-implement", "task_id", t.ID, "pr", pr.Number, "step", step)
	}
}

func (r *Handler) settledImplementationFetchMatchers(ctx context.Context, tasks []task.Task) []github.TaskMatcher {
	matchers := make([]github.TaskMatcher, 0)
	for i := range tasks {
		t := &tasks[i]
		if !staleImplementationWorkflowEligible(t) || t.ProjectID == "" {
			continue
		}
		if r.hasRunningAgentForTask(ctx, t.ID) {
			continue
		}
		m := github.TaskMatcher{
			ID:        t.ID,
			PRNumber:  t.PRNumber,
			Branch:    t.Branch,
			ProjectID: t.ProjectID,
		}
		if m.PRNumber != 0 {
			matchers = append(matchers, m)
			continue
		}
		if m.Branch == "" {
			continue
		}
		head, ok := r.settledImplementationPRHead(ctx, t)
		if !ok {
			continue
		}
		number, found, err := r.findOpenPRForBranch(ctx, m.ProjectID, head)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("pr-monitor.cancel-implement.find-pr", "task_id", m.ID, "repo", m.ProjectID, "head", head, "err", err)
			}
			continue
		}
		if !found {
			continue
		}
		m.PRNumber = number
		matchers = append(matchers, m)
	}
	return matchers
}

func (r *Handler) settledImplementationPRHead(ctx context.Context, t *task.Task) (string, bool) {
	if t == nil || strings.TrimSpace(t.Branch) == "" {
		return "", false
	}
	if r.worktrees == nil {
		return t.Branch, true
	}
	if !r.worktrees.Exists(*t) {
		if r.logger != nil {
			r.logger.Warn("pr-monitor.cancel-implement.no-worktree", "task_id", t.ID, "branch", t.Branch)
		}
		return "", false
	}
	head, err := project.HeadArg(ctx, r.worktrees.PathFor(*t), t.Branch)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("pr-monitor.cancel-implement.head", "task_id", t.ID, "branch", t.Branch, "err", err)
		}
		return "", false
	}
	return head, true
}

func (r *Handler) findOpenPRForBranch(ctx context.Context, repo, branch string) (number int, found bool, err error) {
	if r.findOpenPRForBranchFn != nil {
		return r.findOpenPRForBranchFn(ctx, repo, branch)
	}
	return github.FindPRForBranch(ctx, repo, branch)
}

func (r *Handler) hasRunningAgentForTask(ctx context.Context, taskID string) bool {
	return r.hasBlockingAgentForTask(ctx, taskID)
}

func (r *Handler) hasBlockingAgentForTask(ctx context.Context, taskID string) bool {
	return r.canDispatch(ctx, taskID, "")
}

func (r *Handler) hasBlockingAgentForTaskAllowingAgent(ctx context.Context, taskID, exceptAgentID string) bool {
	return r.canDispatch(ctx, taskID, exceptAgentID)
}

// canDispatch is the single dispatch gate every PR-driven dispatcher (review
// dispatch, PR-fix dispatch, phase reconciliation) checks before starting or
// counting a blocking agent for taskID: is another agent already running (or
// mid-dispatch) for this task? exceptAgentID excludes one specific agent from
// the check — used when the caller IS that agent and only cares about
// siblings — pass "" to check unconditionally.
//
// A stale registration (an agent whose process exited without the manager
// observing it, or a dispatch claim held past its staleness window) is
// released before the final check, so a genuinely idle task is never wedged
// behind bookkeeping that never got cleaned up.
func (r *Handler) canDispatch(ctx context.Context, taskID, exceptAgentID string) bool {
	if r == nil || r.agents == nil {
		return false
	}
	blocked := func() bool {
		if exceptAgentID == "" {
			return r.agents.HasRunningAgentForTask(taskID)
		}
		return r.agents.HasOtherRunningAgentForTask(taskID, exceptAgentID)
	}
	if !blocked() {
		return false
	}
	releasedAgents := r.agents.ReleaseStaleStoppedAgentsForTask(ctx, taskID, stalePRDispatchGateAge)
	releasedClaim := r.agents.ReleaseStaleTaskDispatch(taskID, stalePRDispatchGateAge)
	if releasedAgents > 0 || releasedClaim {
		if r.logger != nil {
			r.logger.Warn("reviews.dispatch.gate.stale-released",
				"task_id", taskID, "agents", releasedAgents, "dispatch_claim", releasedClaim)
		}
		return blocked()
	}
	return true
}

// indexMonitoredPRs builds the by-number and by-branch lookup maps shared by
// every reconciliation pass over a poll cycle's monitoredPRs snapshot.
func indexMonitoredPRs(monitoredPRs []github.PullRequest) (byNumber map[int]*github.PullRequest, byBranch map[string]*github.PullRequest) {
	byNumber = make(map[int]*github.PullRequest, len(monitoredPRs))
	byBranch = make(map[string]*github.PullRequest, len(monitoredPRs))
	for i := range monitoredPRs {
		if monitoredPRs[i].Number > 0 {
			byNumber[monitoredPRs[i].Number] = &monitoredPRs[i]
		}
		if monitoredPRs[i].HeadRefName != "" {
			byBranch[monitoredPRs[i].HeadRefName] = &monitoredPRs[i]
		}
	}
	return byNumber, byBranch
}

func matchingPR(t *task.Task, byNumber map[int]*github.PullRequest, byBranch map[string]*github.PullRequest) *github.PullRequest {
	if t == nil {
		return nil
	}
	// Both maps are repo-blind (keyed by a bare PR number / branch name), so
	// under the global author:@me search (pollAndMonitorPRs) a reused PR number
	// or a same-named branch in a different repo can collide. Reject any
	// candidate whose repo doesn't match the task's project before it can drive
	// a status-mutating reconciler (reconcileHumanRequiredBlockers, #1641).
	if pr := byNumber[t.PRNumber]; sameProjectPR(pr, t) {
		return pr
	}
	if pr := byBranch[t.Branch]; sameProjectPR(pr, t) {
		return pr
	}
	return nil
}

// sameProjectPR reports whether pr belongs to the task's project. A missing
// repo on either side (per-task fetch that doesn't populate Repository, or a
// task without a ProjectID) is treated as a match to preserve the pre-scoping
// behaviour — the guard only ever rejects a positively-mismatched repo.
func sameProjectPR(pr *github.PullRequest, t *task.Task) bool {
	if pr == nil {
		return false
	}
	return t.ProjectID == "" || pr.Repository == "" || pr.Repository == t.ProjectID
}

func (r *Handler) reactivateLinkedOwnPR(t *task.Task, livePR bool) *task.Task {
	if !linkedOwnPRHumanRequiredDrift(t, livePR) {
		return nil
	}
	updated, err := r.tasks.Update(t.ID, task.Update{
		Status:       task.Ptr(task.StatusInReview),
		StatusReason: task.Ptr(""),
	})
	if err != nil {
		r.logger.Error("pr-monitor.reactivate-linked-pr", "task_id", t.ID, "pr", t.PRNumber, "err", err)
		return nil
	}
	r.logger.Info("pr-monitor.reactivate-linked-pr", "task_id", t.ID, "pr", t.PRNumber)
	return &updated
}

func linkedOwnPRHumanRequiredDrift(t *task.Task, livePR bool) bool {
	if t == nil || slices.Contains(t.Tags, "review") {
		return false
	}
	if !livePR || t.Status != task.StatusHumanRequired || t.PRNumber == 0 || strings.TrimSpace(t.StatusReason) != "" {
		return false
	}
	if t.Workflow == nil {
		return false
	}
	if t.Workflow.WorkflowID != "simple-task-pr" || t.Workflow.State != workflow.ExecCompleted || t.Workflow.CompletedAt == nil {
		return false
	}
	completedAt := *t.Workflow.CompletedAt
	if t.UpdatedAt.After(completedAt) {
		return false
	}
	return completedAt.Sub(t.UpdatedAt) <= linkedPRDriftWindow
}

func staleImplementationWorkflowEligible(t *task.Task) bool {
	if t == nil || t.Workflow == nil {
		return false
	}
	switch t.Status {
	case task.StatusInProgress, task.StatusInReview, task.StatusReadyReview:
	default:
		return false
	}
	return t.Workflow.WorkflowID == "simple-task-implement" &&
		t.Workflow.CurrentStep == "implement" &&
		t.Workflow.State != workflow.ExecCompleted &&
		t.Workflow.State != workflow.ExecFailed
}

func settledOwnPR(pr github.PullRequest) bool {
	if pr.IsDraft || pr.Mergeable != "MERGEABLE" || pr.HasPendingChecks {
		return false
	}
	if pr.SourcedViaREST && !pr.RESTCIFetched {
		return false
	}
	return pr.CIStatus == "SUCCESS" || pr.CIStatus == ""
}

// ownPRColumnTask reports whether a task is one of the user's own PRs shown in
// the In Review column — the set reconcilePRPhases assigns a PR phase to.
func ownPRColumnTask(t *task.Task) bool {
	if slices.Contains(t.Tags, "review") {
		return false
	}
	if t.Status != task.StatusInReview && t.Status != task.StatusReadyReview {
		return false
	}
	return t.PRNumber != 0 || t.Branch != ""
}

// applyPRPhase persists the PR phase only when it changed.
func (r *Handler) applyPRPhase(t *task.Task, phase string) {
	if phase == t.PRPhase {
		return
	}
	prev := t.PRPhase
	if _, err := r.tasks.Update(t.ID, task.Update{PRPhase: task.Ptr(phase)}); err != nil {
		r.logger.Error("pr-monitor.phase-update", "task_id", t.ID, "phase", phase, "err", err)
		return
	}
	r.logger.Info("pr-monitor.phase", "task_id", t.ID, "pr", t.PRNumber, "from", prev, "to", phase)
}

// exhaustedFixReasonKind extracts the PRIssueKind escalateExhaustedFix
// recorded in a task's StatusReason (format: "pr-monitor: auto-fix exhausted
// after N attempts (<kind>) — needs a human"), or false if reason doesn't
// match that exact shape. Parsing this instead of free-text matching keeps
// eligibility tied to a reason Sybra itself generated from a live PR signal,
// never to a human- or agent-authored explanation that merely mentions a
// check by name.
const exhaustedFixReasonPrefix = "pr-monitor: auto-fix exhausted after "
const ciInfraRerunPermissionReason = "CI failure rerun requires higher GitHub permissions"

// persistentFlakyCIReason parks a task whose ci_failure kept classifying as
// flaky (see flakyOnlyFailure) across every rerun attempt in the ci-infra
// rerun budget — distinct from exhaustedFixReason so a human can tell "the
// fix agent gave up on a deterministic failure" from "reruns alone never
// cleared this, worth a closer look at test stability" at a glance.
const persistentFlakyCIReason = "pr-monitor: CI failure kept classifying as flaky after " +
	"repeated reruns — needs a human"

// exhaustedFixReason renders the StatusReason escalateExhaustedFix parks a task
// with. It is the sole producer of the string exhaustedFixReasonKind parses back
// into a PRIssueKind — deriving both from exhaustedFixReasonPrefix keeps them in
// lockstep (round-tripped in outbound_test.go), so a wording tweak cannot
// silently stop the reconciler from firing.
func exhaustedFixReason(attempts int, kind github.PRIssueKind) string {
	return fmt.Sprintf("%s%d attempts (%s) — needs a human", exhaustedFixReasonPrefix, attempts, kind)
}

func exhaustedFixReasonKind(reason string) (github.PRIssueKind, bool) {
	rest, ok := strings.CutPrefix(reason, exhaustedFixReasonPrefix)
	if !ok {
		return "", false
	}
	attempts, afterAttempts, ok := strings.Cut(rest, " attempts (")
	if !ok || attempts == "" {
		return "", false
	}
	for i := range attempts {
		if attempts[i] < '0' || attempts[i] > '9' {
			return "", false
		}
	}
	kind, suffixOK := strings.CutSuffix(afterAttempts, ") — needs a human")
	if !suffixOK || kind == "" || strings.ContainsAny(kind, "()") {
		return "", false
	}
	return github.PRIssueKind(kind), true
}

// humanRequiredBlockerReconcilable reports whether t is a blocked or
// human-required task parked solely by a PR blocker a live PR probe can decide
// resolved itself.
// Excludes watchdog stops, tamper flags, comment-review exhaustion (no CI-state
// probe can tell whether reviewer feedback was actually addressed),
// human-authored reasons, and tasks already reconciled once (latch tag present,
// to prevent flip-flopping).
func humanRequiredBlockerReconcilable(t *task.Task) (kind github.PRIssueKind, ok bool) {
	if t == nil || slices.Contains(t.Tags, "review") {
		return "", false
	}
	if (t.Status != task.StatusHumanRequired && t.Status != task.StatusBlocked) || t.PRNumber == 0 {
		return "", false
	}
	if slices.Contains(t.Tags, reconciledLatchTag) {
		return "", false
	}
	if t.Blocker.Kind == blocker.KindReviewFixExhausted && t.Blocker.Actor == blocker.ActorReview {
		kind := github.PRIssueKind(t.Blocker.Code)
		if kind == github.PRIssueCIFailure || kind == github.PRIssueConflict {
			return kind, true
		}
		return "", false
	}
	reason := strings.TrimSpace(t.StatusReason)
	if workflow.IsTamperFlaggedReason(reason) {
		return "", false
	}
	if reason == ciInfraRerunPermissionReason || reason == persistentFlakyCIReason {
		return github.PRIssueCIFailure, true
	}
	kind, ok = exhaustedFixReasonKind(reason)
	if !ok {
		return "", false
	}
	if kind != github.PRIssueCIFailure && kind != github.PRIssueConflict {
		return "", false
	}
	return kind, true
}

// humanRequiredBlockerReconcileEligible reports whether a task should have its
// PR included in the known-PR prefetch for blocker reconciliation.
func humanRequiredBlockerReconcileEligible(t *task.Task) bool {
	_, ok := humanRequiredBlockerReconcilable(t)
	return ok
}

// hasFixableIssue reports whether any issue in the slice is a kind pr-fix
// would act on (conflict, ci_failure, comments) — the set that must be fully
// clear before reconcileHumanRequiredBlockers un-parks a task, so a
// CI-cleared PR that picked up a fresh conflict or review comment in the
// meantime is not resurrected into a state that doesn't reflect its live
// blockers.
func hasFixableIssue(issues []github.PRIssue) bool {
	for i := range issues {
		switch issues[i].Kind {
		case github.PRIssueConflict, github.PRIssueCIFailure, github.PRIssueComments:
			return true
		case github.PRIssueBranchConflictNoPR, github.PRIssueTaskBranchConflict, github.PRIssueBranchRecreate, github.PRIssueReadyToMerge, github.PRIssueCIFlake:
			// branch_conflict_no_pr, task_branch_conflict, and ci_flake
			// are tracker-only (never emitted by MatchTaskPRs);
			// ready_to_merge is not a blocker.
		}
	}
	return false
}

// reconcileHumanRequiredBlockers periodically re-probes human-required tasks
// parked by exhausted ci_failure/conflict auto-fixes against their PR's live
// state. It prefers the poll cycle's monitoredPRs snapshot, but falls back to
// a direct PR-state probe when a task's PR is not in that snapshot yet. If the
// PR is now clearly open, mergeable, green, and free of every fixable issue
// kind, the blocker cleared, so the task is reconciled back to in-review,
// latched against repeat flip-flops, and its retry-tracker entry is cleared so
// a fresh pr-fix budget is available should the same kind recur. When the live
// monitored snapshot already carries the PR, return a same-cycle ready_to_merge
// follow-up so the post-reconcile poll can reuse handleAutoMerge immediately
// instead of leaving a green reviewed pet PR open until the next tick.
func (r *Handler) reconcileHumanRequiredBlockers(tasks []task.Task, monitoredPRs []github.PullRequest) []github.PRIssue {
	byNumber, byBranch := indexMonitoredPRs(monitoredPRs)
	fetchFn := github.FetchPRState
	if r.fetchPRStateFn != nil {
		fetchFn = r.fetchPRStateFn
	}
	var followups []github.PRIssue

	for i := range tasks {
		t := &tasks[i]
		kind, ok := humanRequiredBlockerReconcilable(t)
		if !ok || t.ProjectID == "" {
			continue
		}

		probe := map[string]any{}
		if pr := matchingPR(t, byNumber, byBranch); pr != nil {
			if pr.IsDraft || pr.CIStatus == "PENDING" || pr.HasPendingChecks {
				continue
			}
			issues := github.MatchTaskPRs([]github.PullRequest{*pr}, []github.TaskMatcher{
				{ID: t.ID, PRNumber: t.PRNumber, Branch: t.Branch, ProjectID: t.ProjectID},
			})
			if hasFixableIssue(issues) {
				continue
			}
			probe["state"] = "OPEN"
			probe["mergeable"] = pr.Mergeable
			probe["ci_status"] = pr.CIStatus
		} else {
			state, err := fetchFn(t.ProjectID, t.PRNumber)
			if err != nil {
				r.logger.Info("pr-monitor.reconcile-blocker.probe-failed",
					"task_id", t.ID, "pr", t.PRNumber, "kind", kind, "err", err)
				continue
			}
			ciStatus := state.CIStatus()
			ready := state.State == "OPEN" && state.Mergeable == "MERGEABLE" &&
				ciStatus == "SUCCESS" && !state.HasPendingChecks()
			if !ready {
				r.logger.Info("pr-monitor.reconcile-blocker.not-ready",
					"task_id", t.ID, "pr", t.PRNumber, "kind", kind,
					"state", state.State, "mergeable", state.Mergeable,
					"ci_status", ciStatus, "pending", state.HasPendingChecks())
				continue
			}
			probe["state"] = state.State
			probe["mergeable"] = state.Mergeable
			probe["ci_status"] = ciStatus
		}

		priorReason := t.StatusReason
		preTask := *t
		tags := append(append([]string{}, t.Tags...), reconciledLatchTag)
		updated, err := r.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusInReview),
			StatusReason: task.Ptr(""),
			Tags:         task.Ptr(tags),
		})
		if err != nil {
			r.logger.Error("pr-monitor.reconcile-blocker", "task_id", t.ID, "pr", t.PRNumber, "err", err)
			continue
		}
		tasks[i] = updated
		// One of the two automated exit paths from human-required this
		// package owns — see Handler.recordInterventionOnUnblock. Blocked
		// (not human-required) tasks reconciled here are out of this
		// feature's scope.
		if preTask.Status == task.StatusHumanRequired {
			r.recordInterventionOnUnblock(preTask, string(task.StatusInReview),
				fmt.Sprintf("automatic reconciliation: %s blocker cleared", kind), intervention.OperatorActionAutoRecovery)
		}
		if r.prTracker != nil {
			r.prTracker.Clear(t.ID, kind)
		}
		r.logAudit(audit.EventPRBlockerReconciled, t.ID, "", map[string]any{
			"pr": t.PRNumber, "kind": kind, "prior_reason": priorReason, "probe": probe,
		})
		r.logger.Info("pr-monitor.reconcile-blocker",
			"task_id", t.ID, "pr", t.PRNumber, "kind", kind, "prior_reason", priorReason)
		if pr := matchingPR(t, byNumber, byBranch); pr != nil {
			followups = append(followups, github.PRIssue{
				Kind:   github.PRIssueReadyToMerge,
				TaskID: t.ID,
				PR:     *pr,
			})
		}
	}
	return followups
}
