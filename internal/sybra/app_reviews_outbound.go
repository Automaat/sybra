package sybra

import (
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const linkedPRDriftWindow = 10 * time.Minute

// reconcilePRPhases recomputes the lifecycle phase of every outbound own-PR
// task (status in-review/ready-review, not tag `review`) from the live
// monitored PRs and persists any delta. The phase is a pure overlay on the In
// Review column — it never changes task.Status.
func (r *ReviewHandler) reconcilePRPhases(tasks []task.Task, monitoredPRs []github.PullRequest) {
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

func (r *ReviewHandler) reactivateLinkedOwnPR(t *task.Task, livePR bool) *task.Task {
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
func (r *ReviewHandler) applyPRPhase(t *task.Task, phase string) {
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
