package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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

func (r *ReviewHandler) createReviewTask(pr github.PullRequest, projectID string) {
	r.createReviewTaskWithTriage(pr, projectID, r.triageReview)
}

func (r *ReviewHandler) createReviewTaskWithTriage(pr github.PullRequest, projectID string, triage func(task.Task)) {
	title := "Review: " + pr.Title
	body := fmt.Sprintf("%s\n\nAuthor: @%s", pr.URL, pr.Author)

	t, err := r.tasks.Create(title, body, "headless")
	if err != nil {
		r.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}

	tags := []string{"review"}
	t, err = r.tasks.Update(t.ID, task.Update{
		Tags:      &tags,
		ProjectID: task.Ptr(projectID),
		PRNumber:  task.Ptr(pr.Number),
		Status:    task.Ptr(task.StatusTodo),
	})
	if err != nil {
		r.logger.Error("review.update-task", "task_id", t.ID, "err", err)
		return
	}
	r.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)
	go triage(t)
}

func (r *ReviewHandler) triageReview(t task.Task) {
	stats, err := github.FetchPRStats(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.triage.stats", "task_id", t.ID, "err", err)
		// fallback: start agent when we can't determine size
		if _, err := r.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
			r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
		}
		if err := r.startReviewAgent(t); err != nil {
			r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
		}
		return
	}

	r.logger.Info("review.triage", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)

	if stats.Additions < reviewSmallAdditions && stats.ChangedFiles < reviewSmallFiles {
		reason := fmt.Sprintf("PR too small for agent review (%d additions, %d files)", stats.Additions, stats.ChangedFiles)
		if _, err := r.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: &reason,
		}); err != nil {
			r.logger.Error("review.triage.human", "task_id", t.ID, "err", err)
		}
		r.logger.Info("review.triage.small", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)
		return
	}

	if _, err := r.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
		r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
	}
	if err := r.startReviewAgent(t); err != nil {
		r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
	}
}

func (r *ReviewHandler) startFixReviewAgent(t task.Task) error {
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", t.ID)
	}

	dir, err := r.worktrees.PrepareForFix(t, t.PRNumber)
	if err != nil {
		return fmt.Errorf("prepare worktree: %w", err)
	}

	prompt := fmt.Sprintf(
		"Run /fix-review https://github.com/%s/pull/%d --auto\n\n"+
			"IMPORTANT: when committing, use conventional commit format "+
			"`fix(review): address PR review comments` (type(scope) required by repo hooks). "+
			"Sign the commit with `git commit -s -S`. Push the branch when done.",
		t.ProjectID, t.PRNumber,
	)

	ag, err := r.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RoleFixReview.AgentName(t.Title),
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "opus",
		// MaxTurns intentionally not inherited: fix-review agents need
		// enough turns to fetch the PR, apply fixes, and commit.
	})
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleFixReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
		Prompt: prompt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	r.logAudit(audit.EventFixReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	r.logger.Info("fix-review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (r *ReviewHandler) startReviewAgent(t task.Task) error {
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", t.ID)
	}

	dir := config.HomeDir()
	if t.ProjectID != "" {
		d, err := r.worktrees.PrepareForReview(t)
		if err != nil {
			r.logger.Error("review.worktree", "task_id", t.ID, "err", err)
		} else {
			dir = d
		}
	}

	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)

	ag, err := r.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RoleReview.AgentName(t.Title),
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "opus",
		// MaxTurns intentionally not inherited: review agents need
		// enough turns to fetch the PR, run the skill, and write findings.
	})
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
		Prompt: prompt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	r.logAudit(audit.EventReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	r.logger.Info("review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (r *ReviewHandler) maybeCreateReviewTasks(tasks []task.Task, reviewPRs []github.PullRequest) {
	projects, err := r.projects.List()
	if err != nil || len(projects) == 0 {
		return
	}

	projectMatchers := make([]github.ProjectMatcher, 0, len(projects))
	for i := range projects {
		projectMatchers = append(projectMatchers, github.ProjectMatcher{
			ID:         projects[i].Owner + "/" + projects[i].Repo,
			Repository: projects[i].Owner + "/" + projects[i].Repo,
		})
	}

	matches := github.MatchReviewPRs(reviewPRs, projectMatchers)
	for i := range matches {
		if matches[i].PR.IsDraft {
			continue
		}
		if matches[i].PR.ReviewDecision == "APPROVED" {
			continue
		}
		if r.hasReviewTask(tasks, matches[i].PR.Number) {
			continue
		}
		r.createReviewTask(matches[i].PR, matches[i].ProjectID)
	}
}

func (r *ReviewHandler) hasReviewTask(tasks []task.Task, prNumber int) bool {
	for i := range tasks {
		if tasks[i].PRNumber == prNumber && slices.Contains(tasks[i].Tags, "review") {
			return true
		}
	}
	return false
}

func (r *ReviewHandler) detectPublishedReviews(tasks []task.Task) {
	for i := range tasks {
		if tasks[i].Status != task.StatusHumanRequired {
			continue
		}
		if !slices.Contains(tasks[i].Tags, "review") {
			continue
		}
		if tasks[i].PRNumber == 0 || tasks[i].ProjectID == "" {
			continue
		}

		pending, err := github.HasPendingReview(tasks[i].ProjectID, tasks[i].PRNumber)
		if err != nil {
			r.logger.Warn("review.poll-pending", "task_id", tasks[i].ID, "err", err)
			continue
		}
		if !pending {
			if _, err := r.tasks.Update(tasks[i].ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
				r.logger.Error("review.published-update", "task_id", tasks[i].ID, "err", err)
				continue
			}
			r.logAudit(audit.EventReviewPublished, tasks[i].ID, "", map[string]any{"pr": tasks[i].PRNumber})
			r.logger.Info("review.published", "task_id", tasks[i].ID, "pr", tasks[i].PRNumber)
		}
	}
}

func reviewClosedPREligible(t *task.Task) bool {
	return t.TaskType != task.TaskTypeChat &&
		!task.IsTerminalStatus(t.Status) &&
		slices.Contains(t.Tags, "review") &&
		t.ProjectID != "" &&
		t.PRNumber != 0
}

func reviewTaskMatchers(tasks []task.Task) []github.TaskMatcher {
	matchers := make([]github.TaskMatcher, 0, len(tasks))
	for i := range tasks {
		if !reviewClosedPREligible(&tasks[i]) {
			continue
		}
		matchers = append(matchers, github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			ProjectID: tasks[i].ProjectID,
		})
	}
	return matchers
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

	var matchers []github.TaskMatcher
	for i := range tasks {
		if !prMonitorEligible(&tasks[i]) {
			continue
		}
		matchers = append(matchers, github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			Branch:    tasks[i].Branch,
			ProjectID: tasks[i].ProjectID,
		})
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

		closedPRs := github.DetectClosedTaskPRs(monitoredPRs, matchers, github.FetchPRState)
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

func openReviewPRs(summary github.ReviewSummary) []github.PullRequest {
	if len(summary.ReviewedByMe) == 0 {
		return summary.ReviewRequested
	}
	prs := make([]github.PullRequest, 0, len(summary.ReviewRequested)+len(summary.ReviewedByMe))
	prs = append(prs, summary.ReviewRequested...)
	prs = append(prs, summary.ReviewedByMe...)
	return prs
}

func (r *ReviewHandler) closeFinishedReviewTasks(tasks []task.Task, openReviewPRs []github.PullRequest) {
	matchers := reviewTaskMatchers(tasks)
	if len(matchers) == 0 {
		return
	}
	closedPRs := github.DetectClosedTaskPRs(openReviewPRs, matchers, github.FetchPRState)
	for _, c := range closedPRs {
		if r.agents.HasRunningAgentForTask(c.TaskID) {
			r.logger.Info("review.closed-skip-running-agent", "task_id", c.TaskID, "pr", c.PRNumber)
			continue
		}
		reason := fmt.Sprintf("review PR %s", strings.ToLower(c.State))
		if _, err := r.tasks.Update(c.TaskID, task.Update{
			Status:       task.Ptr(task.StatusDone),
			StatusReason: &reason,
		}); err != nil {
			r.logger.Error("review.closed-update", "task_id", c.TaskID, "err", err)
			continue
		}
		eventType := audit.EventPRMerged
		if c.State == "CLOSED" {
			eventType = audit.EventPRClosed
		}
		r.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State, "review_task": true})
		r.logger.Info("review.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State)
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

func (r *ReviewHandler) handleAutoMerge(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.Type != project.ProjectTypePet {
		return
	}

	if err := github.MergePR(issue.PR.Repository, issue.PR.Number); err != nil {
		r.logger.Error("auto-merge.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", err)
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRAutoMerged, t.ID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	})
	r.logger.Info("auto-merge.merged", "task_id", t.ID, "pr", issue.PR.Number)
}

func (r *ReviewHandler) handlePRIssue(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	var prompt string
	switch issue.Kind {
	case github.PRIssueConflict:
		prompt = conflictPrompt(issue.PR)
		r.logAudit(audit.EventPRConflictDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueCIFailure:
		prompt = fmt.Sprintf(
			"Fix failing CI on branch `%s` (PR #%d). "+
				"Check the failing run with `gh run view --log-failed`, "+
				"fix the code, commit and push. No unrelated changes.\n\n"+
				"Push to the same remote the PR was opened from — never "+
				"to `origin` when a `fork` remote exists:\n"+
				"```sh\n"+
				"PUSH_REMOTE=origin\n"+
				"if git config --get remote.fork.url >/dev/null; then "+
				"PUSH_REMOTE=fork; fi\n"+
				"git push \"$PUSH_REMOTE\" HEAD:%s\n"+
				"```",
			issue.PR.HeadRefName, issue.PR.Number, issue.PR.HeadRefName,
		)
		r.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueReadyToMerge:
		// handled by handleAutoMerge, not by agent spawn
		return
	}

	dir := ""
	if t.ProjectID != "" {
		var d string
		var wtErr error
		if issue.Kind == github.PRIssueConflict {
			d, wtErr = r.worktrees.PrepareForFix(t, issue.PR.Number)
		} else {
			d, wtErr = r.worktrees.PrepareForTask(t, nil)
		}
		if wtErr != nil {
			r.logger.Error("pr-monitor.worktree", "task_id", t.ID, "err", wtErr)
			return
		}
		dir = d
	}

	if r.workflowEngine == nil {
		r.logger.Error("pr-monitor.no-workflow-engine", "task_id", t.ID)
		return
	}

	// Dispatch pr.event through the engine so trigger conditions in the
	// workflow YAML stay authoritative. StartWorkflow would bypass them.
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s", t.Title, prompt)
	vars := map[string]string{
		"prompt":                fullPrompt,
		"pr_issue_kind":         string(issue.Kind),
		workflow.WorkflowVarDir: dir,
	}
	wfID, err := r.workflowEngine.DispatchEvent(t.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(issue.Kind)}, vars)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			r.logger.Info("pr-monitor.workflow-already-active",
				"task_id", t.ID, "kind", string(issue.Kind))
			return
		}
		r.logger.Error("pr-monitor.workflow-dispatch", "task_id", t.ID, "err", err)
		return
	}
	if wfID == "" {
		r.logger.Warn("pr-monitor.no-matching-workflow",
			"task_id", t.ID, "kind", string(issue.Kind))
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, "", map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number, "workflow": wfID,
	})

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "workflow", wfID,
	)
}

func conflictPrompt(pr github.PullRequest) string {
	var filesCtx string
	if files, err := github.FetchPRFiles(pr.Repository, pr.Number); err == nil && len(files) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\nFiles changed in this PR:\n")
		for _, f := range files {
			sb.WriteString("- ")
			sb.WriteString(f)
			sb.WriteByte('\n')
		}
		filesCtx = sb.String()
	}

	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (PR #%d). "+
			"Do NOT investigate git state — go straight to rebasing.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git rebase refs/remotes/origin/main\n"+
			"# resolve each conflict, git add, git rebase --continue\n"+
			"PUSH_REMOTE=origin\n"+
			"if git config --get remote.fork.url >/dev/null; then PUSH_REMOTE=fork; fi\n"+
			"git push --force-with-lease \"$PUSH_REMOTE\" HEAD:%s\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `refs/remotes/origin/main` (not `origin/main`) to avoid ambiguous refs\n"+
			"- Push to `fork` (not `origin`) when a `fork` remote exists — the PR was opened from the fork\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- If rebase produces more than 3 conflicting files, run `git rebase --abort` and stop — the task needs human review\n"+
			"- No investigation, no extra commits, no unrelated changes"+
			"%s",
		pr.HeadRefName, pr.Number, pr.HeadRefName, filesCtx,
	)
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
