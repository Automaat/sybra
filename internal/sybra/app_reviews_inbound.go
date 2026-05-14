package sybra

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

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
