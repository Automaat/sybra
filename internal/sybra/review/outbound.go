package review

import (
	"regexp"
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
// (which would otherwise see the same task, now back in-review, cycle to
// human-required again on a fresh unrelated failure, and re-match the old
// reason text) never double-reconciles the same escalation.
const reconciledLatchTag = "monitor:reconciled"

// prMonitorExhaustedReasonRe matches the status_reason text escalateExhaustedFix
// writes when a pr-fix retry budget is spent, capturing the issue kind.
var prMonitorExhaustedReasonRe = regexp.MustCompile(`^pr-monitor: auto-fix exhausted after \d+ attempts \((\w+)\) — needs a human$`)

// humanRequiredBlockerReconcilable reports whether t is a human-required task
// parked solely by the pr-monitor auto-fix-exhausted escalation for a
// ci_failure or conflict issue — the only kinds a live PR probe can decide
// resolved itself. Excludes watchdog stops, tamper flags, comment-review
// exhaustion (no CI-state probe can tell whether reviewer feedback was
// actually addressed), human-authored reasons, and tasks already reconciled
// once (latch tag present, to prevent flip-flopping).
func humanRequiredBlockerReconcilable(t *task.Task) (kind string, ok bool) {
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
	m := prMonitorExhaustedReasonRe.FindStringSubmatch(reason)
	if m == nil {
		return "", false
	}
	kind = m[1]
	if kind != string(github.PRIssueCIFailure) && kind != string(github.PRIssueConflict) {
		return "", false
	}
	return kind, true
}

// reconcileHumanRequiredBlockers re-probes the linked PR of every task
// eligible per humanRequiredBlockerReconcilable and moves it back to
// in-review when the PR is unambiguously ready: open, mergeable, CI green,
// and nothing still pending. Any other outcome (pending/failed/unknown/empty
// CI, not mergeable, closed, or a fetch error) leaves the task parked — an
// empty CIStatus must fail closed here (unlike the general-purpose
// PRState.Resolved(), which treats it as passing) because a task landed in
// human-required specifically for a CI failure, so "no CI signal at all" is
// far more likely a fetch/config gap than a genuinely check-less PR.
func (r *Handler) reconcileHumanRequiredBlockers(tasks []task.Task) {
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

		priorReason := t.StatusReason
		tags := append(append([]string{}, t.Tags...), reconciledLatchTag)
		updated, err := r.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusInReview),
			StatusReason: task.Ptr(""),
			Tags:         task.Ptr(tags),
		})
		if err != nil {
			r.logger.Error("pr-monitor.reconcile-blocker", "task_id", t.ID, "err", err)
			continue
		}
		tasks[i] = updated

		r.logAudit(audit.EventPRBlockerReconciled, t.ID, "", map[string]any{
			"pr": t.PRNumber, "kind": kind, "prior_reason": priorReason,
			"probe": map[string]any{
				"state": state.State, "mergeable": state.Mergeable, "ci_status": ciStatus,
			},
		})
		r.logger.Info("pr-monitor.reconcile-blocker",
			"task_id", t.ID, "pr", t.PRNumber, "kind", kind, "prior_reason", priorReason)
	}
}

// reconcilePRPhases recomputes the lifecycle phase of every outbound own-PR
// task (status in-review/ready-review, not tag `review`) from the live
// monitored PRs and persists any delta. The phase is a pure overlay on the In
// Review column — it never changes task.Status.
func (r *Handler) reconcilePRPhases(tasks []task.Task, monitoredPRs []github.PullRequest) {
	byNumber := make(map[int]*github.PullRequest, len(monitoredPRs))
	byBranch := make(map[string]*github.PullRequest, len(monitoredPRs))
	for i := range monitoredPRs {
		if monitoredPRs[i].Number > 0 {
			byNumber[monitoredPRs[i].Number] = &monitoredPRs[i]
		}
		if monitoredPRs[i].HeadRefName != "" {
			byBranch[monitoredPRs[i].HeadRefName] = &monitoredPRs[i]
		}
	}

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
