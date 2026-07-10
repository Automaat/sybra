package recovery

import (
	"context"
	"slices"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// ReconcileLostPRNumber self-heals in-review tasks whose pr_number is 0: with
// no linked PR the landing detector can never match a merged PR, so the task
// orphans at in-review while every recovery pass re-derives that status from
// its stale workflow. A merged PR (resolved by branch, then linked issue)
// advances the task to done and clears the workflow so the status sticks; an
// open PR backfills pr_number; no match is left untouched. A nil PRs resolver
// makes this a no-op so it never blocks a machine without GitHub configured.
func (r *Recovery) ReconcileLostPRNumber(ctx context.Context) {
	if r.PRs == nil {
		return
	}
	tasks, err := r.Tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if !reconcileLostPREligible(t) {
			continue
		}
		if r.Agents.HasRunningAgentForTask(t.ID) || r.Agents.IsDispatching(t.ID) {
			continue
		}
		ref, resolveErr := r.PRs.ResolvePRForTask(ctx, t.ProjectID, t.Branch, t.Issue)
		if resolveErr != nil {
			r.Throttle.Log(r.Logger, "reconcile-lost-pr.resolve", "reconcile:"+t.ID, resolveErr, "task_id", t.ID)
			continue
		}
		if ref.Number <= 0 {
			continue
		}
		switch ref.State {
		case "MERGED":
			r.reconcileLandedTask(t, ref.Number)
		case "OPEN":
			r.reconcileBackfillPR(t, ref.Number)
		}
	}
}

func (r *Recovery) reconcileLandedTask(t *task.Task, prNumber int) {
	var clearWorkflow *workflow.Execution
	if _, err := r.Tasks.Update(t.ID, task.Update{
		PRNumber:     task.Ptr(prNumber),
		Status:       task.Ptr(task.StatusDone),
		Outcome:      task.Ptr("merged"),
		StatusReason: task.Ptr(""),
		Workflow:     &clearWorkflow,
	}); err != nil {
		r.Logger.Error("reconcile-lost-pr.landed", "task_id", t.ID, "pr", prNumber, "err", err)
		return
	}
	r.Logger.Info("reconcile-lost-pr.landed", "task_id", t.ID, "pr", prNumber)
}

func (r *Recovery) reconcileBackfillPR(t *task.Task, prNumber int) {
	if _, err := r.Tasks.Update(t.ID, task.Update{PRNumber: task.Ptr(prNumber)}); err != nil {
		r.Logger.Error("reconcile-lost-pr.backfill", "task_id", t.ID, "pr", prNumber, "err", err)
		return
	}
	r.Logger.Info("reconcile-lost-pr.backfilled", "task_id", t.ID, "pr", prNumber)
}

func reconcileLostPREligible(t *task.Task) bool {
	if t.Status != task.StatusInReview {
		return false
	}
	if t.PRNumber != 0 || t.Branch == "" || t.ProjectID == "" {
		return false
	}
	if t.TaskType == task.TaskTypeChat || t.TaskType == task.TaskTypeUmbrella {
		return false
	}
	return !slices.Contains(t.Tags, "review")
}
