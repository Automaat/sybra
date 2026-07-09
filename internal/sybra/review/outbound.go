package review

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
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
func (r *Handler) reconcilePRPhases(tasks []task.Task, monitoredPRs []github.PullRequest) {
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
			AgentRunning:     r.agents.HasRunningAgentForTask(t.ID),
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
	if t == nil || t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
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

// ownPRColumnTask reports whether a task is one of the user's own PRs shown in
// the In Review column — the set reconcilePRPhases assigns a PR phase to.
func ownPRColumnTask(t *task.Task) bool {
	if t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
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

// humanRequiredBlockerReconcilable reports whether t is a human-required task
// parked solely by the pr-monitor auto-fix-exhausted escalation for a
// ci_failure or conflict issue — the only kinds a live PR probe can decide
// resolved itself. Excludes watchdog stops, tamper flags, comment-review
// exhaustion (no CI-state probe can tell whether reviewer feedback was
// actually addressed), human-authored reasons, and tasks already reconciled
// once (latch tag present, to prevent flip-flopping).
func humanRequiredBlockerReconcilable(t *task.Task) (kind github.PRIssueKind, ok bool) {
	if t == nil || t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return "", false
	}
	if t.Status != task.StatusHumanRequired || t.PRNumber == 0 {
		return "", false
	}
	if slices.Contains(t.Tags, reconciledLatchTag) {
		return "", false
	}
	reason := strings.TrimSpace(t.StatusReason)
	if workflow.IsTamperFlaggedReason(reason) {
		return "", false
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
		case github.PRIssueBranchConflictNoPR, github.PRIssueReadyToMerge:
			// branch_conflict_no_pr is tracker-only (never emitted by
			// MatchTaskPRs); ready_to_merge is not a blocker.
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
// a fresh pr-fix budget is available should the same kind recur.
func (r *Handler) reconcileHumanRequiredBlockers(tasks []task.Task, monitoredPRs []github.PullRequest) {
	byNumber, byBranch := indexMonitoredPRs(monitoredPRs)
	fetchFn := github.FetchPRState
	if r.fetchPRStateFn != nil {
		fetchFn = r.fetchPRStateFn
	}

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
		if r.prTracker != nil {
			r.prTracker.Clear(t.ID, kind)
		}
		r.logAudit(audit.EventPRBlockerReconciled, t.ID, "", map[string]any{
			"pr": t.PRNumber, "kind": kind, "prior_reason": priorReason, "probe": probe,
		})
		r.logger.Info("pr-monitor.reconcile-blocker",
			"task_id", t.ID, "pr", t.PRNumber, "kind", kind, "prior_reason", priorReason)
	}
}
