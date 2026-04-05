package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
)

const (
	prPollFast = 1 * time.Minute
	prPollSlow = 5 * time.Minute
)

const (
	reviewSmallAdditions = 200
	reviewSmallFiles     = 5
)

// ReviewHandler manages PR review task creation, agent dispatch, and status tracking.
type ReviewHandler struct {
	tasks     *task.Store
	projects  *project.Store
	agents    *agent.Manager
	audit     *audit.Logger
	logger    *slog.Logger
	prTracker *github.IssueTracker
	emit      func(string, any)
	agentOrch *AgentOrchestrator
}

func newReviewHandler(
	tasks *task.Store,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	prTracker *github.IssueTracker,
	emit func(string, any),
	agentOrch *AgentOrchestrator,
) *ReviewHandler {
	return &ReviewHandler{
		tasks:     tasks,
		projects:  projects,
		agents:    agents,
		audit:     al,
		logger:    logger,
		prTracker: prTracker,
		emit:      emit,
		agentOrch: agentOrch,
	}
}

func (r *ReviewHandler) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if r.audit == nil {
		return
	}
	if err := r.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		r.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

func (r *ReviewHandler) createReviewTask(pr github.PullRequest, projectID string) {
	title := "Review: " + pr.Title
	body := fmt.Sprintf("%s\n\nAuthor: @%s", pr.URL, pr.Author)

	t, err := r.tasks.Create(title, body, "headless")
	if err != nil {
		r.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}

	if _, err := r.tasks.Update(t.ID, map[string]any{
		"tags":       "review",
		"project_id": projectID,
		"pr_number":  pr.Number,
		"status":     string(task.StatusTodo),
	}); err != nil {
		r.logger.Error("review.update-task", "task_id", t.ID, "err", err)
		return
	}
	r.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)
	go r.triageReview(t)
}

func (r *ReviewHandler) triageReview(t task.Task) {
	stats, err := github.FetchPRStats(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.triage.stats", "task_id", t.ID, "err", err)
		// fallback: start agent when we can't determine size
		if _, err := r.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
		}
		if err := r.startReviewAgent(t); err != nil {
			r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
		}
		return
	}

	r.logger.Info("review.triage", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)

	if stats.Additions < reviewSmallAdditions && stats.ChangedFiles < reviewSmallFiles {
		if _, err := r.tasks.Update(t.ID, map[string]any{
			"status":        string(task.StatusHumanRequired),
			"status_reason": fmt.Sprintf("PR too small for agent review (%d additions, %d files)", stats.Additions, stats.ChangedFiles),
		}); err != nil {
			r.logger.Error("review.triage.human", "task_id", t.ID, "err", err)
		}
		r.logger.Info("review.triage.small", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)
		return
	}

	if _, err := r.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
		r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
	}
	if err := r.startReviewAgent(t); err != nil {
		r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
	}
}

// StartReview is exposed as a Wails-bound method.
func (a *App) StartReview(taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", taskID)
	}
	return a.reviewer.startReviewAgent(t)
}

func (r *ReviewHandler) startReviewAgent(t task.Task) error {
	dir := config.HomeDir()
	if t.ProjectID != "" {
		d, err := r.prepareReviewWorktree(t)
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
	})
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	r.logAudit(audit.EventReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	r.logger.Info("review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (r *ReviewHandler) prepareReviewWorktree(t task.Task) (string, error) {
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		r.logger.Warn("review.worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := github.FetchPRBranch(t.ProjectID, t.PRNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr branch: %w", err)
	}

	wtPath := filepath.Join(r.agentOrch.worktreesDir, t.DirName())
	if _, statErr := os.Stat(wtPath); statErr == nil {
		return wtPath, nil
	}

	ref := "refs/remotes/origin/" + branch
	if err := project.CreateWorktreeDetached(proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create review worktree: %w", err)
	}

	r.logger.Info("review.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
}

// prepareFixWorktree checks out the PR's head branch so the agent can rebase and push.
func (r *ReviewHandler) prepareFixWorktree(t task.Task, prNumber int) (string, error) {
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		r.logger.Warn("fix.worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := github.FetchPRBranch(t.ProjectID, prNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr branch: %w", err)
	}

	wtPath := filepath.Join(r.agentOrch.worktreesDir, t.DirName())

	// Remove stale worktree — previous agent may have left dirty state.
	if _, statErr := os.Stat(wtPath); statErr == nil {
		_ = project.RemoveWorktree(proj.ClonePath, wtPath)
	}

	ref := "refs/remotes/origin/" + branch
	if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create fix worktree: %w", err)
	}

	if err := project.SanitizeWorktree(wtPath); err != nil {
		r.logger.Warn("fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}

	r.logger.Info("fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
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
			if _, err := r.tasks.Update(tasks[i].ID, map[string]any{
				"status": string(task.StatusInReview),
			}); err != nil {
				r.logger.Error("review.published-update", "task_id", tasks[i].ID, "err", err)
				continue
			}
			r.logAudit(audit.EventReviewPublished, tasks[i].ID, "", map[string]any{"pr": tasks[i].PRNumber})
			r.logger.Info("review.published", "task_id", tasks[i].ID, "pr", tasks[i].PRNumber)
		}
	}
}

func (r *ReviewHandler) pollAndMonitorPRs() time.Duration {
	summary, err := github.FetchReviews()
	if err != nil {
		r.logger.Warn("pr-monitor.fetch", "err", err)
		return prPollSlow
	}

	r.emit("reviews:updated", summary)

	tasks, err := r.tasks.List()
	if err != nil {
		return prPollSlow
	}

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
		r.prTracker.Cleanup()

		for i := range issues {
			if r.agents.HasRunningAgentForTask(issues[i].TaskID) {
				continue
			}
			if !r.prTracker.ShouldHandle(issues[i].TaskID, issues[i].Kind) {
				continue
			}
			r.handlePRIssue(issues[i])
		}

		closedPRs := github.DetectClosedTaskPRs(summary.CreatedByMe, matchers, github.FetchPRState)
		for _, c := range closedPRs {
			if _, err := r.tasks.Update(c.TaskID, map[string]any{"status": string(task.StatusDone)}); err != nil {
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

	if prNeedsAttention(summary.CreatedByMe) {
		return prPollFast
	}
	return prPollSlow
}

func (r *ReviewHandler) handlePRIssue(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	if _, err := r.tasks.Update(t.ID, map[string]any{
		"status": string(task.StatusInProgress),
	}); err != nil {
		r.logger.Error("pr-monitor.status-update", "task_id", t.ID, "err", err)
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
				"Do NOT investigate git state — go straight to the failure.\n\n"+
				"```bash\n"+
				"gh run list --branch %s --limit 3\n"+
				"gh run view <FAILED_RUN_ID> --log-failed\n"+
				"```\n\n"+
				"Read the failure, fix the code, commit and push. No unrelated changes.",
			issue.PR.HeadRefName, issue.PR.Number,
			issue.PR.HeadRefName,
		)
		r.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})
	}

	dir := ""
	if t.ProjectID != "" {
		var d string
		var wtErr error
		if issue.Kind == github.PRIssueConflict {
			d, wtErr = r.prepareFixWorktree(t, issue.PR.Number)
		} else {
			d, wtErr = r.agentOrch.prepareWorktree(t)
		}
		if wtErr != nil {
			r.logger.Error("pr-monitor.worktree", "task_id", t.ID, "err", wtErr)
			return
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s", t.Title, prompt)
	ag, err := r.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RolePRFix.AgentName(t.Title),
		Mode:   "headless",
		Prompt: fullPrompt,
		Dir:    dir,
		Model:  "sonnet",
	})
	if err != nil {
		r.logger.Error("pr-monitor.agent-start", "task_id", t.ID, "err", err)
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind)
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, ag.ID, map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number,
	})

	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: "headless",
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		r.logger.Error("pr-monitor.add-run", "task_id", t.ID, "err", err)
	}

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "agent_id", ag.ID,
	)
}

func (a *App) ListReviewComments(taskID string) ([]task.ReviewComment, error) {
	return a.tasks.Comments().List(taskID)
}

func (a *App) AddReviewComment(taskID string, line int, body string) (task.ReviewComment, error) {
	return a.tasks.Comments().Add(taskID, line, body)
}

func (a *App) ResolveReviewComment(taskID, commentID string) error {
	return a.tasks.Comments().Resolve(taskID, commentID)
}

func (a *App) DeleteReviewComment(taskID, commentID string) error {
	return a.tasks.Comments().Delete(taskID, commentID)
}

func conflictPrompt(pr github.PullRequest) string {
	filesCtx := ""
	if files, err := github.FetchPRFiles(pr.Repository, pr.Number); err == nil && len(files) > 0 {
		filesCtx = "\n\nFiles changed in this PR:\n"
		for _, f := range files {
			filesCtx += "- " + f + "\n"
		}
	}

	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (PR #%d). "+
			"Do NOT investigate git state — go straight to rebasing.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git rebase refs/remotes/origin/main\n"+
			"# resolve each conflict, git add, git rebase --continue\n"+
			"git push --force-with-lease\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `refs/remotes/origin/main` (not `origin/main`) to avoid ambiguous refs\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- If rebase produces more than 3 conflicting files, run `git rebase --abort` and stop — the task needs human review\n"+
			"- No investigation, no extra commits, no unrelated changes"+
			"%s",
		pr.HeadRefName, pr.Number, filesCtx,
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
	}
	return false
}
