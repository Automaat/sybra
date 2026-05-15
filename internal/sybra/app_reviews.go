package sybra

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

const (
	prPollFast = 1 * time.Minute
	prPollSlow = 5 * time.Minute
)

const (
	reviewSmallAdditions = 200
	reviewSmallFiles     = 5
)

const transientFetchWarnThreshold = 3

// ReviewHandler manages PR review task creation, agent dispatch, and status tracking.
type ReviewHandler struct {
	DomainHandler
	tasks          *task.Manager
	projects       *project.Store
	agents         *agent.Manager
	prTracker      *github.IssueTracker
	worktrees      *worktree.Manager
	workflowEngine *workflow.Engine
	// renovatePRsFn returns Renovate-bot PRs to fold into the monitor pass.
	// FetchReviews uses author:@me which excludes bot-authored PRs, so without
	// this hook a Renovate PR linked to a task by pr_number/branch never gets
	// re-dispatched to pr-fix when CI fails. nil = renovate disabled.
	renovatePRsFn       func() []github.PullRequest
	transientFetchFails int
}

func newReviewHandler(
	tasks *task.Manager,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	prTracker *github.IssueTracker,
	emit func(string, any),
	worktrees *worktree.Manager,
	renovatePRsFn func() []github.PullRequest,
) *ReviewHandler {
	return &ReviewHandler{
		DomainHandler: DomainHandler{audit: al, logger: logger, emit: emit},
		tasks:         tasks,
		projects:      projects,
		agents:        agents,
		prTracker:     prTracker,
		worktrees:     worktrees,
		renovatePRsFn: renovatePRsFn,
	}
}

func (r *ReviewHandler) Name() string { return "reviews" }

func (r *ReviewHandler) Poll(_ context.Context) time.Duration {
	return r.pollAndMonitorPRs()
}

func (r *ReviewHandler) pollAndMonitorPRs() time.Duration {
	summary, err := github.FetchReviews()
	if err != nil {
		if github.IsTransientError(err) {
			r.transientFetchFails++
			if r.transientFetchFails < transientFetchWarnThreshold {
				r.logger.Info("pr-monitor.fetch", "err", err)
			} else {
				r.logger.Warn("pr-monitor.fetch", "err", err, "consecutive", r.transientFetchFails)
			}
		} else {
			r.transientFetchFails = 0
			r.logger.Warn("pr-monitor.fetch", "err", err)
		}
		return prPollSlow
	}
	r.transientFetchFails = 0

	r.emit("reviews:updated", summary)

	tasks, err := r.tasks.List()
	if err != nil {
		return prPollSlow
	}

	var (
		matchers       []github.TaskMatcher
		closedMatchers []github.TaskMatcher
	)
	for i := range tasks {
		m := github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			Branch:    tasks[i].Branch,
			ProjectID: tasks[i].ProjectID,
		}
		if prMonitorEligible(&tasks[i]) {
			matchers = append(matchers, m)
			closedMatchers = append(closedMatchers, m)
		} else if prClosedEligible(&tasks[i]) {
			// human-required tasks are excluded from pr-fix dispatch but still
			// advance to done when their PR is merged.
			closedMatchers = append(closedMatchers, m)
		}
	}

	monitoredPRs := r.monitoredPRs(summary)

	if len(matchers) > 0 {
		issues := github.MatchTaskPRs(monitoredPRs, matchers)
		r.prTracker.Cleanup()
		r.cancelResolvedPRFixWorkflows(tasks, issues)

		for i := range issues {
			if r.agents.HasRunningAgentForTask(issues[i].TaskID) {
				continue
			}
			// Gate dispatch on workflow state too: an agent may have just
			// exited while the workflow is still in verify_commits /
			// link_pr_and_review. Without this, a fresh issue (e.g.
			// kind=conflict appearing because main moved during the agent
			// run) races the in-flight workflow's tail steps and triggers
			// a layered re-dispatch that DispatchEvent later rejects, but
			// only after we've prepped a worktree and emitted audit
			// noise.
			if r.workflowEngine != nil && r.workflowEngine.HasActiveWorkflow(issues[i].TaskID) {
				continue
			}
			if !r.prTracker.ShouldHandle(issues[i].TaskID, issues[i].Kind, issues[i].PR.HeadSHA) {
				continue
			}
			if issues[i].Kind == github.PRIssueReadyToMerge {
				r.handleAutoMerge(issues[i])
				continue
			}
			r.handlePRIssue(issues[i])
		}
	}

	if len(closedMatchers) > 0 {
		closedPRs := github.DetectClosedTaskPRs(monitoredPRs, closedMatchers, github.FetchPRState)
		for _, c := range closedPRs {
			if r.agents.HasRunningAgentForTask(c.TaskID) {
				r.logger.Info("pr-monitor.closed-skip-running-agent", "task_id", c.TaskID, "pr", c.PRNumber)
				continue
			}
			if _, err := r.tasks.Update(c.TaskID, task.Update{Status: task.Ptr(task.StatusDone)}); err != nil {
				r.logger.Error("pr-monitor.closed-update", "task_id", c.TaskID, "err", err)
				continue
			}
			eventType := audit.EventPRMerged
			if c.State == "CLOSED" {
				eventType = audit.EventPRClosed
			}
			r.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State})
			r.logger.Info("pr-monitor.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State)
		}
	}

	r.maybeCreateReviewTasks(tasks, summary.ReviewRequested)
	r.detectPublishedReviews(tasks)
	r.closeFinishedReviewTasks(tasks, openReviewPRs(summary))

	if prNeedsAttention(monitoredPRs) {
		return prPollFast
	}
	return prPollSlow
}

// cancelResolvedPRFixWorkflows terminates any in-flight pr-fix workflow
// whose originating PR issue (`pr_issue_kind` in workflow vars) is no
// longer present on the live PR. Prevents ResumeStalled from re-spawning
// fix agents forever when the underlying CI failure or conflict has
// since been resolved on a newer push.
//
// Without this, a pr-fix workflow remains in state=waiting on the `fix`
// step until its agent succeeds or the task is deleted — there is no
// trigger-re-evaluation between dispatch and completion. The orchestrator
// loop then re-dispatches the step every minute, spawning a fresh agent
// each time even though the PR is now green.
func (r *ReviewHandler) cancelResolvedPRFixWorkflows(tasks []task.Task, issues []github.PRIssue) {
	if r.workflowEngine == nil {
		return
	}
	// Index live issues per task so we can answer "kind K still present
	// for task T?" in O(1).
	liveByTask := make(map[string]map[string]bool, len(tasks))
	for i := range issues {
		set := liveByTask[issues[i].TaskID]
		if set == nil {
			set = make(map[string]bool, 2)
			liveByTask[issues[i].TaskID] = set
		}
		set[string(issues[i].Kind)] = true
	}

	for i := range tasks {
		t := &tasks[i]
		if t.Workflow == nil || t.Workflow.WorkflowID != "pr-fix" {
			continue
		}
		if t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed {
			continue
		}
		kind := t.Workflow.Variables["pr_issue_kind"]
		if kind == "" {
			continue
		}
		if liveByTask[t.ID][kind] {
			continue // condition still holds — let the workflow proceed
		}
		step, err := r.workflowEngine.CancelWorkflow(t.ID, "pr-monitor: "+kind+" resolved")
		if err != nil {
			r.logger.Error("pr-monitor.cancel-resolved", "task_id", t.ID, "kind", kind, "err", err)
			continue
		}
		// Clear cooldown so a future failure of the same kind on a new
		// SHA re-triggers fresh (the closed-PR path does the same via
		// prTracker.Cleanup; we need the explicit clear here because
		// the PR is still open).
		r.prTracker.Clear(t.ID, github.PRIssueKind(kind))
		r.logger.Info("pr-monitor.cancel-resolved",
			"task_id", t.ID, "kind", kind, "step", step, "pr", t.PRNumber)
	}
}

// monitoredPRs returns the union of user-authored PRs (from FetchReviews) and
// Renovate-bot PRs (from renovatePRsFn). Renovate's PRs aren't in author:@me,
// so without folding them in here the pr-fix monitor would never re-spawn an
// agent on a Renovate PR whose CI keeps failing.
func (r *ReviewHandler) monitoredPRs(summary github.ReviewSummary) []github.PullRequest {
	if r.renovatePRsFn == nil {
		return summary.CreatedByMe
	}
	renovatePRs := r.renovatePRsFn()
	if len(renovatePRs) == 0 {
		return summary.CreatedByMe
	}
	prs := make([]github.PullRequest, 0, len(summary.CreatedByMe)+len(renovatePRs))
	prs = append(prs, summary.CreatedByMe...)
	prs = append(prs, renovatePRs...)
	return prs
}

// prMonitorEligible decides whether the PR monitor should consider a task
// when scanning for CI failures, conflicts, and ready-to-merge state.
//
// Historical behavior was "in-review only" — which silently stranded tasks
// whose workflow exited to `in-progress` with a live PR (e.g. an evaluate
// step that crashed before flipping to in-review, or a manually-spawned
// agent that opened a PR outside of any workflow). Those tasks would render
// a red ✗ in the kanban UI forever and never get picked up for pr-fix.
//
// Now we also include in-progress tasks that carry an explicit PR number.
// Branch-only matching stays gated on in-review to avoid false positives
// from tasks that pushed a WIP branch without opening a PR yet.
func prMonitorEligible(t *task.Task) bool {
	if t.TaskType == task.TaskTypeChat {
		// Chat tasks are ephemeral and never have PRs — exclude from PR monitoring.
		return false
	}
	if slices.Contains(t.Tags, "review") {
		// Review tasks are inbound (reviewing someone else's PR), not tasks
		// whose own PR is being tracked. They're handled separately.
		return false
	}
	switch t.Status {
	case task.StatusInReview:
		return t.PRNumber != 0 || t.Branch != ""
	case task.StatusInProgress:
		// Only in-progress tasks that already have a PR — a branch alone
		// isn't enough, we don't want to treat mid-implementation tasks
		// as candidates for pr-fix dispatch.
		return t.PRNumber != 0
	default:
		return false
	}
}

// prClosedEligible is a superset of prMonitorEligible: it additionally includes
// human-required tasks that carry a PR number. Those tasks are excluded from
// pr-fix dispatch and auto-merge (they need operator attention) but should
// still advance to done when their PR is merged or closed.
func prClosedEligible(t *task.Task) bool {
	if prMonitorEligible(t) {
		return true
	}
	if t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return false
	}
	return t.Status == task.StatusHumanRequired && t.PRNumber != 0
}

func prNeedsAttention(prs []github.PullRequest) bool {
	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return true
		}
		if prs[i].Mergeable == "CONFLICTING" || prs[i].Mergeable == "UNKNOWN" {
			return true
		}
		if !prs[i].IsDraft && prs[i].Mergeable == "MERGEABLE" && (prs[i].CIStatus == "SUCCESS" || prs[i].CIStatus == "") {
			return true
		}
	}
	return false
}
