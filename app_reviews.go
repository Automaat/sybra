package main

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
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
	DomainHandler
	tasks          *task.Manager
	projects       *project.Store
	agents         *agent.Manager
	prTracker      *github.IssueTracker
	worktrees      *worktree.Manager
	workflowEngine *workflow.Engine
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
) *ReviewHandler {
	return &ReviewHandler{
		DomainHandler: DomainHandler{audit: al, logger: logger, emit: emit},
		tasks:         tasks,
		projects:      projects,
		agents:        agents,
		prTracker:     prTracker,
		worktrees:     worktrees,
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

	tags := []string{"review"}
	if _, err := r.tasks.Update(t.ID, task.Update{
		Tags:      &tags,
		ProjectID: task.Ptr(projectID),
		PRNumber:  task.Ptr(pr.Number),
		Status:    task.Ptr(task.StatusTodo),
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
			"Sign the commit with `git commit -s -S`.",
		t.ProjectID, t.PRNumber,
	)

	ag, err := r.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RoleFixReview.AgentName(t.Title),
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "opus",
	})
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleFixReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	r.logAudit(audit.EventFixReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	r.logger.Info("fix-review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (r *ReviewHandler) startReviewAgent(t task.Task) error {
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
			if issues[i].Kind == github.PRIssueReadyToMerge {
				r.handleAutoMerge(issues[i])
				continue
			}
			r.handlePRIssue(issues[i])
		}

		closedPRs := github.DetectClosedTaskPRs(summary.CreatedByMe, matchers, github.FetchPRState)
		for _, c := range closedPRs {
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

	if prNeedsAttention(summary.CreatedByMe) {
		return prPollFast
	}
	return prPollSlow
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

	r.prTracker.MarkHandled(t.ID, issue.Kind)
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
				"fix the code, commit and push. No unrelated changes.",
			issue.PR.HeadRefName, issue.PR.Number,
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
			d, wtErr = r.worktrees.PrepareForTask(t)
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

	r.prTracker.MarkHandled(t.ID, issue.Kind)
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, "", map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number, "workflow": wfID,
	})

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "workflow", wfID,
	)
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
		if !prs[i].IsDraft && prs[i].Mergeable == "MERGEABLE" && (prs[i].CIStatus == "SUCCESS" || prs[i].CIStatus == "") {
			return true
		}
	}
	return false
}
