package main

import (
	"context"
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
)

const (
	renovatePollFast = 1 * time.Minute
	renovatePollSlow = 5 * time.Minute
)

// RenovateHandler manages Renovate PR polling and actions.
type RenovateHandler struct {
	projects *project.Store
	logger   *slog.Logger
	emit     func(string, any)
	cfg      *config.RenovateConfig
}

func newRenovateHandler(
	projects *project.Store,
	logger *slog.Logger,
	emit func(string, any),
	cfg *config.RenovateConfig,
) *RenovateHandler {
	return &RenovateHandler{
		projects: projects,
		logger:   logger,
		emit:     emit,
		cfg:      cfg,
	}
}

func (h *RenovateHandler) repos() []string {
	projects, err := h.projects.List()
	if err != nil {
		h.logger.Error("renovate.list-projects", "err", err)
		return nil
	}
	repos := make([]string, 0, len(projects))
	for i := range projects {
		repos = append(repos, projects[i].Owner+"/"+projects[i].Repo)
	}
	return repos
}

func (h *RenovateHandler) pollRenovatePRs() time.Duration {
	repos := h.repos()
	if len(repos) == 0 {
		return renovatePollSlow
	}

	prs, err := github.FetchRenovatePRs(h.cfg.Author, repos)
	if err != nil {
		h.logger.Warn("renovate.fetch", "err", err)
		return renovatePollSlow
	}

	h.emit("renovate:updated", prs)
	h.logger.Debug("renovate.poll", "count", len(prs))

	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return renovatePollFast
		}
	}
	return renovatePollSlow
}

func (a *App) initRenovate(emit func(string, any)) {
	if !a.cfg.Renovate.Enabled {
		return
	}
	a.renovateHandler = newRenovateHandler(a.projects, a.logger, emit, &a.cfg.Renovate)
	a.logger.Info("renovate.enabled", "author", a.cfg.Renovate.Author)
}

// FetchRenovatePRs is exposed as a Wails-bound method for manual refresh.
func (a *App) FetchRenovatePRs() ([]github.RenovatePR, error) {
	if a.renovateHandler == nil {
		return nil, nil
	}
	repos := a.renovateHandler.repos()
	if len(repos) == 0 {
		return nil, nil
	}
	return github.FetchRenovatePRs(a.cfg.Renovate.Author, repos)
}

// MergeRenovatePR merges a Renovate PR.
func (a *App) MergeRenovatePR(repo string, number int) error {
	return github.MergePR(repo, number)
}

// ApproveRenovatePR approves a Renovate PR.
func (a *App) ApproveRenovatePR(repo string, number int) error {
	return github.ApprovePR(repo, number)
}

// RerunRenovateChecks reruns failed CI checks on a Renovate PR.
func (a *App) RerunRenovateChecks(repo string, number int) error {
	return github.RerunFailedChecks(repo, number)
}

// FixRenovateCI spawns an agent to fix CI failures on a Renovate PR.
func (a *App) FixRenovateCI(repo string, number int, branch, title string) error {
	// Check for existing fix task for this PR.
	tasks, err := a.tasks.List()
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	for i := range tasks {
		t := &tasks[i]
		if t.PRNumber == number && t.ProjectID == repo && slices.Contains(t.Tags, "renovate-fix") {
			if t.Status != task.StatusDone && a.agents.HasRunningAgentForTask(t.ID) {
				return nil // already being fixed
			}
		}
	}

	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	t, err := a.tasks.Create("Fix CI: "+title, prURL, "headless")
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{
		"project_id": repo,
		"pr_number":  number,
		"tags":       "renovate-fix",
		"run_role":   "pr-fix",
	}); err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	t, _ = a.tasks.Get(t.ID)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.worktrees.PrepareForFix(t, number)
		if wtErr != nil {
			a.logger.Error("renovate-fix.worktree", "task_id", t.ID, "err", wtErr)
			return fmt.Errorf("prepare worktree: %w", wtErr)
		}
		dir = d
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{
		"status": string(task.StatusInProgress),
	}); err != nil {
		a.logger.Error("renovate-fix.status", "task_id", t.ID, "err", err)
	}

	prompt := fmt.Sprintf(
		"# Task: Fix CI: %s\n\n"+
			"Fix failing CI on branch `%s` (PR #%d). "+
			"Check the failing run with `gh run view --log-failed`, "+
			"fix the code, commit and push. No unrelated changes.",
		title, branch, number,
	)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RolePRFix.AgentName(t.Title),
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "sonnet",
	})
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: "headless",
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("renovate-fix.add-run", "task_id", t.ID, "err", err)
	}

	if a.audit != nil {
		_ = a.audit.Log(audit.Event{
			Type:    audit.EventRenovateCIFix,
			TaskID:  t.ID,
			AgentID: ag.ID,
			Data:    map[string]any{"pr": number, "repo": repo},
		})
	}

	a.logger.Info("renovate-fix.started", "task_id", t.ID, "agent_id", ag.ID, "pr", number)
	return nil
}

func (a *App) renovatePollLoop(ctx context.Context) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := a.renovateHandler.pollRenovatePRs()
			a.logger.Debug("renovate-poll.next", "interval", next)
			timer.Reset(next)
		}
	}
}
