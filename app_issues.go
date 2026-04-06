package main

import (
	"context"
	"time"

	"github.com/Automaat/synapse/internal/github"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const issuesPollInterval = 5 * time.Minute

// FetchAssignedIssues is exposed as a Wails-bound method for manual refresh.
func (a *App) FetchAssignedIssues() ([]github.Issue, error) {
	return github.FetchAssignedIssues()
}

func (a *App) issuesPollLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			issues, err := github.FetchAssignedIssues()
			if err != nil {
				a.logger.Warn("issues.fetch", "err", err)
				timer.Reset(issuesPollInterval)
				continue
			}

			runtime.EventsEmit(a.ctx, "issues:updated", issues)
			a.logger.Debug("issues.poll", "count", len(issues))

			a.syncIssuesToTasks(issues)
			timer.Reset(issuesPollInterval)
		}
	}
}

func (a *App) syncIssuesToTasks(issues []github.Issue) {
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Error("issue-sync.list-tasks", "err", err)
		return
	}

	issueURLs := make(map[string]struct{})
	for i := range tasks {
		if tasks[i].Issue != "" {
			issueURLs[tasks[i].Issue] = struct{}{}
		}
	}

	for i := range issues {
		issue := &issues[i]
		if _, exists := issueURLs[issue.URL]; exists {
			continue
		}

		t, err := a.tasks.Create(issue.Title, issue.Body, "headless")
		if err != nil {
			a.logger.Error("issue-sync.create", "issue", issue.URL, "err", err)
			continue
		}

		updates := map[string]any{
			"issue":  issue.URL,
			"status": "todo",
		}

		if _, projErr := a.projects.Get(issue.Repository); projErr == nil {
			updates["project_id"] = issue.Repository
		}

		if _, err := a.tasks.Update(t.ID, updates); err != nil {
			a.logger.Error("issue-sync.update", "task_id", t.ID, "err", err)
		}

		a.logger.Info("issue-sync.created", "task_id", t.ID, "issue", issue.URL)
	}
}
