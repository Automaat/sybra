package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	prPollFast = 1 * time.Minute
	prPollSlow = 5 * time.Minute
)

func (a *App) FetchReviews() (github.ReviewSummary, error) {
	return github.FetchReviews()
}

func (a *App) MarkPRReady(repo string, number int) error {
	return github.MarkReady(repo, number)
}

const (
	reviewSmallAdditions = 200
	reviewSmallFiles     = 5
)

func (a *App) createReviewTask(pr github.PullRequest, projectID string) {
	title := "Review: " + pr.Title
	body := fmt.Sprintf("%s\n\nAuthor: @%s", pr.URL, pr.Author)

	t, err := a.tasks.Create(title, body, "headless")
	if err != nil {
		a.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}

	t, err = a.tasks.Update(t.ID, map[string]any{
		"tags":       "review",
		"project_id": projectID,
		"pr_number":  pr.Number,
		"status":     string(task.StatusTodo),
	})
	if err != nil {
		a.logger.Error("review.update-task", "task_id", t.ID, "err", err)
		return
	}
	a.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)

	a.wg.Go(func() { a.triageReview(t) })
}

func (a *App) triageReview(t task.Task) {
	stats, err := github.FetchPRStats(t.ProjectID, t.PRNumber)
	if err != nil {
		a.logger.Warn("review.triage.stats", "task_id", t.ID, "err", err)
		// fallback: start agent when we can't determine size
		if _, err := a.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			a.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
		}
		if err := a.startReviewAgent(t); err != nil {
			a.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
		}
		return
	}

	a.logger.Info("review.triage", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)

	if stats.Additions < reviewSmallAdditions && stats.ChangedFiles < reviewSmallFiles {
		if _, err := a.tasks.Update(t.ID, map[string]any{"status": string(task.StatusHumanRequired)}); err != nil {
			a.logger.Error("review.triage.human", "task_id", t.ID, "err", err)
		}
		a.logger.Info("review.triage.small", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)
		return
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
		a.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
	}
	if err := a.startReviewAgent(t); err != nil {
		a.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
	}
}

func (a *App) StartReview(taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", taskID)
	}
	return a.startReviewAgent(t)
}

func (a *App) startReviewAgent(t task.Task) error {
	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   "review:" + t.Title,
		Mode:   "headless",
		Prompt: prompt,
		Dir:    config.HomeDir(),
		Model:  "opus",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "review", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logAudit(audit.EventReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	a.logger.Info("review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (a *App) resolveReviewStatus(taskID string) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			a.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	pending, err := github.HasPendingReview(t.ProjectID, t.PRNumber)
	if err != nil {
		a.logger.Warn("review.pending-check", "task_id", taskID, "err", err)
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			a.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	nextStatus := task.StatusInReview
	if pending {
		nextStatus = task.StatusHumanRequired
	}
	if _, err := a.tasks.Update(taskID, map[string]any{"status": string(nextStatus)}); err != nil {
		a.logger.Error("review.status-update", "task_id", taskID, "err", err)
	}
}

func (a *App) prPollLoop(ctx context.Context) {
	timer := time.NewTimer(10 * time.Second) // initial fetch shortly after startup
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := a.pollAndMonitorPRs()
			a.logger.Debug("pr-poll.next", "interval", next)
			timer.Reset(next)
		}
	}
}

func (a *App) pollAndMonitorPRs() time.Duration {
	summary, err := github.FetchReviews()
	if err != nil {
		a.logger.Warn("pr-monitor.fetch", "err", err)
		return prPollSlow
	}

	runtime.EventsEmit(a.ctx, "reviews:updated", summary)

	tasks, err := a.tasks.List()
	if err != nil {
		return prPollSlow
	}

	// Monitor PRs created by the user (conflicts, CI, merged/closed)
	var matchers []github.TaskMatcher
	for i := range tasks {
		if tasks[i].Status != task.StatusInReview {
			continue
		}
		if tasks[i].PRNumber == 0 && tasks[i].Branch == "" {
			continue
		}
		matchers = append(matchers, github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			Branch:    tasks[i].Branch,
			ProjectID: tasks[i].ProjectID,
		})
	}

	if len(matchers) > 0 {
		issues := github.MatchTaskPRs(summary.CreatedByMe, matchers)
		a.prTracker.Cleanup()

		for i := range issues {
			if a.agents.HasRunningAgentForTask(issues[i].TaskID) {
				continue
			}
			if !a.prTracker.ShouldHandle(issues[i].TaskID, issues[i].Kind) {
				continue
			}
			a.handlePRIssue(issues[i])
		}

		closedPRs := github.DetectClosedTaskPRs(summary.CreatedByMe, matchers, github.FetchPRState)
		for _, c := range closedPRs {
			if _, err := a.tasks.Update(c.TaskID, map[string]any{"status": string(task.StatusDone)}); err != nil {
				a.logger.Error("pr-monitor.closed-update", "task_id", c.TaskID, "err", err)
				continue
			}
			eventType := audit.EventPRMerged
			if c.State == "CLOSED" {
				eventType = audit.EventPRClosed
			}
			a.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State})
			a.logger.Info("pr-monitor.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State)
		}
	}

	// Auto-create review tasks from review-requested PRs
	a.maybeCreateReviewTasks(tasks, summary.ReviewRequested)

	// Detect published reviews (human-required → in-review)
	a.detectPublishedReviews(tasks)

	if prNeedsAttention(summary.CreatedByMe) {
		return prPollFast
	}
	return prPollSlow
}

func prNeedsAttention(prs []github.PullRequest) bool {
	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return true
		}
		if prs[i].Mergeable == "CONFLICTING" || prs[i].Mergeable == "UNKNOWN" {
			return true
		}
	}
	return false
}

func (a *App) maybeCreateReviewTasks(tasks []task.Task, reviewPRs []github.PullRequest) {
	projects, err := a.projects.List()
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
		if a.hasReviewTask(tasks, matches[i].PR.Number) {
			continue
		}
		a.createReviewTask(matches[i].PR, matches[i].ProjectID)
	}
}

func (a *App) hasReviewTask(tasks []task.Task, prNumber int) bool {
	for i := range tasks {
		if tasks[i].PRNumber == prNumber && slices.Contains(tasks[i].Tags, "review") {
			return true
		}
	}
	return false
}

func (a *App) detectPublishedReviews(tasks []task.Task) {
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
			a.logger.Warn("review.poll-pending", "task_id", tasks[i].ID, "err", err)
			continue
		}
		if !pending {
			if _, err := a.tasks.Update(tasks[i].ID, map[string]any{
				"status": string(task.StatusInReview),
			}); err != nil {
				a.logger.Error("review.published-update", "task_id", tasks[i].ID, "err", err)
				continue
			}
			a.logAudit(audit.EventReviewPublished, tasks[i].ID, "", map[string]any{"pr": tasks[i].PRNumber})
			a.logger.Info("review.published", "task_id", tasks[i].ID, "pr", tasks[i].PRNumber)
		}
	}
}

func (a *App) handlePRIssue(issue github.PRIssue) {
	t, err := a.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{
		"status": string(task.StatusInProgress),
	}); err != nil {
		a.logger.Error("pr-monitor.status-update", "task_id", t.ID, "err", err)
		return
	}

	var prompt string
	switch issue.Kind {
	case github.PRIssueConflict:
		prompt = fmt.Sprintf(
			"Fix merge conflicts on branch `%s` (PR #%d). "+
				"Do NOT investigate — go straight to fixing.\n\n"+
				"```bash\n"+
				"git fetch origin main\n"+
				"git rebase origin/main\n"+
				"# resolve each conflict, git add, git rebase --continue\n"+
				"git push --force-with-lease\n"+
				"```\n\n"+
				"Resolve conflicts to keep BOTH sides' changes. Push when done.",
			issue.PR.HeadRefName, issue.PR.Number,
		)
		a.logAudit(audit.EventPRConflictDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueCIFailure:
		prompt = fmt.Sprintf(
			"Fix failing CI on branch `%s` (PR #%d). "+
				"Do NOT investigate git state — go straight to the failure.\n\n"+
				"```bash\n"+
				"gh run list --branch %s --limit 3\n"+
				"gh run view <FAILED_RUN_ID> --log-failed\n"+
				"```\n\n"+
				"Read the failure, fix the code, commit and push. No unrelated changes.",
			issue.PR.HeadRefName, issue.PR.Number,
			issue.PR.HeadRefName,
		)
		a.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})
	}

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			a.logger.Error("pr-monitor.worktree", "task_id", t.ID, "err", wtErr)
			return
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s", t.Title, prompt)
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   "pr-fix:" + t.Title,
		Mode:   "headless",
		Prompt: fullPrompt,
		Dir:    dir,
		Model:  "sonnet",
	})
	if err != nil {
		a.logger.Error("pr-monitor.agent-start", "task_id", t.ID, "err", err)
		return
	}

	a.prTracker.MarkHandled(t.ID, issue.Kind)
	a.logAudit(audit.EventPRFixAgentStarted, t.ID, ag.ID, map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number,
	})

	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "pr-fix", Mode: "headless",
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("pr-monitor.add-run", "task_id", t.ID, "err", err)
	}

	a.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "agent_id", ag.ID,
	)
}

// referenced to satisfy project import (used via a.projects field type)
var _ *project.Store
