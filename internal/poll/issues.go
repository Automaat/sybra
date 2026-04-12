package poll

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
)

const IssuesPollInterval = 5 * time.Minute

// synapseIssueLabel is the GitHub label that triggers auto-creation of Synapse tasks.
const synapseIssueLabel = "synapse"

// IssuesFetcher polls GitHub for assigned and labeled issues and syncs them to tasks.
type IssuesFetcher struct {
	tasks         *task.Manager
	projects      *project.Store
	emit          func(string, any)
	logger        *slog.Logger
	allowsType    func(project.ProjectType) bool
	fetchAssigned func() ([]github.Issue, error)
	fetchLabeled  func(repos []string, label string) ([]github.Issue, error)
}

// NewIssuesFetcher creates an IssuesFetcher. allowsType filters issues whose
// repository is registered as a project — if it returns false for that
// project's type, the issue is skipped. A nil closure means "allow all types".
func NewIssuesFetcher(
	tasks *task.Manager,
	projects *project.Store,
	emit func(string, any),
	logger *slog.Logger,
	allowsType func(project.ProjectType) bool,
) *IssuesFetcher {
	if allowsType == nil {
		allowsType = func(project.ProjectType) bool { return true }
	}
	return &IssuesFetcher{
		tasks:         tasks,
		projects:      projects,
		emit:          emit,
		logger:        logger,
		allowsType:    allowsType,
		fetchAssigned: github.FetchAssignedIssues,
		fetchLabeled:  github.FetchLabeledIssuesForRepos,
	}
}

func (f *IssuesFetcher) Name() string { return "issues" }

func (f *IssuesFetcher) Poll(_ context.Context) time.Duration {
	issues, err := f.fetchAssigned()
	if err != nil {
		f.logger.Warn("issues.fetch", "err", err)
		return IssuesPollInterval
	}
	f.emit("issues:updated", issues)
	f.logger.Debug("issues.poll", "count", len(issues))
	f.syncIssuesToTasks(issues)
	f.syncLabeledIssuesToTasks()
	return IssuesPollInterval
}

// syncLabeledIssuesToTasks fetches issues labeled 'synapse' across all registered
// pet projects and creates tasks for any not yet tracked.
func (f *IssuesFetcher) syncLabeledIssuesToTasks() {
	projects, err := f.projects.List()
	if err != nil {
		f.logger.Error("labeled-issues.list-projects", "err", err)
		return
	}

	var repos []string
	for i := range projects {
		if f.allowsType(projects[i].Type) {
			repos = append(repos, projects[i].ID)
		}
	}
	if len(repos) == 0 {
		return
	}

	labeled, err := f.fetchLabeled(repos, synapseIssueLabel)
	if err != nil {
		f.logger.Warn("labeled-issues.fetch", "err", err)
		return
	}
	f.logger.Debug("labeled-issues.poll", "count", len(labeled))
	f.syncIssuesToTasks(labeled)
}

func (f *IssuesFetcher) syncIssuesToTasks(issues []github.Issue) {
	tasks, err := f.tasks.List()
	if err != nil {
		f.logger.Error("issue-sync.list-tasks", "err", err)
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

		// Skip issues from registered projects whose type isn't allowed on this
		// machine. Issues from unregistered repos pass through (caller hasn't
		// declared a type for them).
		if proj, err := f.projects.Get(issue.Repository); err == nil && !f.allowsType(proj.Type) {
			continue
		}

		// Task already exists with the issue URL as title (manually created).
		// Enrich it with the real title and link instead of creating a duplicate.
		if taskID, exists := urlTitleTasks[issue.URL]; exists {
			u := task.Update{
				Title: task.Ptr(issue.Title),
				Issue: task.Ptr(issue.URL),
			}
			if issue.Body != "" {
				u.Body = task.Ptr(issue.Body)
			}
			if _, projErr := f.projects.Get(issue.Repository); projErr == nil {
				u.ProjectID = task.Ptr(issue.Repository)
			}
			if _, err := f.tasks.Update(taskID, u); err != nil {
				f.logger.Error("issue-sync.enrich", "task_id", taskID, "err", err)
			} else {
				f.logger.Info("issue-sync.enriched", "task_id", taskID, "issue", issue.URL, "title", issue.Title)
			}
			continue
		}

		t, err := f.tasks.Create(issue.Title, issue.Body, "headless")
		if err != nil {
			f.logger.Error("issue-sync.create", "issue", issue.URL, "err", err)
			continue
		}

		u := task.Update{
			Issue:  task.Ptr(issue.URL),
			Status: task.Ptr(task.StatusTodo),
		}

		if _, projErr := f.projects.Get(issue.Repository); projErr == nil {
			u.ProjectID = task.Ptr(issue.Repository)
		}

		if len(issue.Labels) > 0 {
			labels := issue.Labels
			u.Tags = &labels
		}

		if _, err := f.tasks.Update(t.ID, u); err != nil {
			f.logger.Error("issue-sync.update", "task_id", t.ID, "err", err)
		}

		f.logger.Info("issue-sync.created", "task_id", t.ID, "issue", issue.URL)
	}
}
