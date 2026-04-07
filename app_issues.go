package main

import (
	"context"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const issuesPollInterval = 5 * time.Minute

// synapseIssueLabel is the GitHub label that triggers auto-creation of Synapse tasks.
const synapseIssueLabel = "synapse"

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
			a.syncLabeledIssuesToTasks()
			timer.Reset(issuesPollInterval)
		}
	}
}

// syncLabeledIssuesToTasks fetches issues labeled 'synapse' across all registered
// pet projects and creates tasks for any not yet tracked.
func (a *App) syncLabeledIssuesToTasks() {
	projects, err := a.projects.List()
	if err != nil {
		a.logger.Error("labeled-issues.list-projects", "err", err)
		return
	}

	var repos []string
	for i := range projects {
		if projects[i].Type == project.ProjectTypePet {
			repos = append(repos, projects[i].ID)
		}
	}
	if len(repos) == 0 {
		return
	}

	labeled, err := github.FetchLabeledIssuesForRepos(repos, synapseIssueLabel)
	if err != nil {
		a.logger.Warn("labeled-issues.fetch", "err", err)
		return
	}
	a.logger.Debug("labeled-issues.poll", "count", len(labeled))
	a.syncIssuesToTasks(labeled)
}

func (a *App) syncIssuesToTasks(issues []github.Issue) {
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Error("issue-sync.list-tasks", "err", err)
		return
	}

	issueURLs := make(map[string]struct{})
	// Map URL-titled tasks so we can enrich them instead of creating duplicates.
	urlTitleTasks := make(map[string]string) // issue URL → task ID
	for i := range tasks {
		if tasks[i].Issue != "" {
			issueURLs[tasks[i].Issue] = struct{}{}
		}
		if strings.HasPrefix(tasks[i].Title, "https://github.com/") {
			urlTitleTasks[tasks[i].Title] = tasks[i].ID
		}
	}

	for i := range issues {
		issue := &issues[i]
		if _, exists := issueURLs[issue.URL]; exists {
			continue
		}

		// Task already exists with the issue URL as title (manually created).
		// Enrich it with the real title and link instead of creating a duplicate.
		if taskID, exists := urlTitleTasks[issue.URL]; exists {
			updates := map[string]any{
				"title": issue.Title,
				"issue": issue.URL,
			}
			if issue.Body != "" {
				updates["body"] = issue.Body
			}
			if _, projErr := a.projects.Get(issue.Repository); projErr == nil {
				updates["project_id"] = issue.Repository
			}
			if _, err := a.tasks.Update(taskID, updates); err != nil {
				a.logger.Error("issue-sync.enrich", "task_id", taskID, "err", err)
			} else {
				a.logger.Info("issue-sync.enriched", "task_id", taskID, "issue", issue.URL, "title", issue.Title)
			}
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

		if len(issue.Labels) > 0 {
			updates["tags"] = issue.Labels
		}

		if _, err := a.tasks.Update(t.ID, updates); err != nil {
			a.logger.Error("issue-sync.update", "task_id", t.ID, "err", err)
		}

		a.logger.Info("issue-sync.created", "task_id", t.ID, "issue", issue.URL)
	}
}
