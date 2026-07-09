package review

import (
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const linkedPRDriftWindow = 10 * time.Minute

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
	pr := byNumber[t.PRNumber]
	if pr == nil {
		pr = byBranch[t.Branch]
	}
	return pr
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

func exhaustedFixReasonKind(reason string) (github.PRIssueKind, bool) {
	if !strings.HasPrefix(reason, exhaustedFixReasonPrefix) {
		return "", false
	}
	open := strings.IndexByte(reason, '(')
	closeIdx := strings.IndexByte(reason, ')')
	if open < 0 || closeIdx <= open+1 {
		return "", false
	}
	return github.PRIssueKind(reason[open+1 : closeIdx]), true
}

// humanRequiredBlockerReconcileEligible reports whether a human-required task
// is a candidate for a live re-probe of its original blocker. Scoped tightly
// to tasks escalateExhaustedFix parked on an exhausted ci_failure fix (a
// failing named check — e.g. DCO — or CI job): that reason names a specific,
// deterministic, externally-observable fact, so a clean re-probe is proof the
// blocker itself cleared (#1641). Every other human-required reason (a draft
// review awaiting a human's verification, a dwell escalation, a deliberate
// watchdog stop, exhausted conflict/comments needing a rebase or judgment
// call) requires a human decision to unpark and is left untouched.
func humanRequiredBlockerReconcileEligible(t *task.Task) bool {
	if t == nil || t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return false
	}
	if t.Status != task.StatusHumanRequired || t.PRNumber == 0 {
		return false
	}
	kind, ok := exhaustedFixReasonKind(t.StatusReason)
	return ok && kind == github.PRIssueCIFailure
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
// parked on an exhausted CI-failure fix (humanRequiredBlockerReconcileEligible)
// against their PR's live state. prMonitorEligible excludes human-required
// tasks from pr-fix dispatch entirely, so without this pass nothing ever
// re-checks whether the original blocking check (e.g. DCO) has since gone
// green — the task sits on a stale reason indefinitely (#1641). If the PR is
// currently free of every fixable issue kind, the blocker cleared, so
// reconcile the task back to in-review and clear its retry-tracker entry so a
// fresh pr-fix budget is available should the same kind of issue recur.
func (r *Handler) reconcileHumanRequiredBlockers(tasks []task.Task, monitoredPRs []github.PullRequest) {
	byNumber, byBranch := indexMonitoredPRs(monitoredPRs)
	for i := range tasks {
		t := &tasks[i]
		if !humanRequiredBlockerReconcileEligible(t) {
			continue
		}
		pr := matchingPR(t, byNumber, byBranch)
		if pr == nil || pr.IsDraft {
			// Not found this cycle (closed/merged, or not yet surfaced by the
			// fetch) or still a draft — leave parked, re-probe next poll.
			continue
		}
		issues := github.MatchTaskPRs([]github.PullRequest{*pr}, []github.TaskMatcher{
			{ID: t.ID, PRNumber: t.PRNumber, Branch: t.Branch, ProjectID: t.ProjectID},
		})
		if hasFixableIssue(issues) {
			continue
		}
		updated, err := r.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusInReview),
			StatusReason: task.Ptr(""),
		})
		if err != nil {
			r.logger.Error("pr-monitor.reconcile-human-required", "task_id", t.ID, "pr", t.PRNumber, "err", err)
			continue
		}
		tasks[i] = updated
		if r.prTracker != nil {
			r.prTracker.Clear(t.ID, github.PRIssueCIFailure)
		}
		r.logAudit(audit.EventPRHumanRequiredReconciled, t.ID, "", map[string]any{
			"pr": t.PRNumber, "repo": t.ProjectID,
		})
		r.logger.Info("pr-monitor.reconcile-human-required", "task_id", t.ID, "pr", t.PRNumber)
	}
}
