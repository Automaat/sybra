package poll

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

const IssuesPollInterval = 5 * time.Minute

const issuesTransientWarnThreshold = 3

// synapseIssueLabel is the GitHub label that triggers auto-creation of Sybra tasks.
const synapseIssueLabel = "sybra"

// IssuesFetcher polls GitHub for assigned and labeled issues and syncs them to tasks.
type IssuesFetcher struct {
	tasks                 *task.Manager
	projects              *project.Store
	emit                  func(string, any)
	logger                *slog.Logger
	allowsType            func(project.ProjectType) bool
	fetchAssigned         func() ([]github.Issue, error)
	fetchLabeled          func(repos []string, label string) ([]github.Issue, error)
	fetchSnapshot         func(repos []string, label string) (github.IssueSnapshot, error)
	transientFetchFails   int
	transientLabeledFails int
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
		fetchSnapshot: github.FetchIssueSnapshot,
	}
}

func (f *IssuesFetcher) Name() string { return "issues" }

func (f *IssuesFetcher) Poll(_ context.Context) time.Duration {
	if f.fetchSnapshot != nil {
		f.pollSnapshot()
		return IssuesPollInterval
	}

	issues, err := f.fetchAssigned()
	metrics.GitHubFetch(err == nil)
	if err != nil {
		if github.IsTransientError(err) {
			f.transientFetchFails++
			if f.transientFetchFails < issuesTransientWarnThreshold {
				f.logger.Info("issues.fetch", "err", err)
			} else {
				f.logger.Warn("issues.fetch", "err", err, "consecutive", f.transientFetchFails)
			}
		} else {
			f.transientFetchFails = 0
			f.logger.Warn("issues.fetch", "err", err)
		}
		return IssuesPollInterval
	}
	f.transientFetchFails = 0
	f.emit("issues:updated", issues)
	f.logger.Debug("issues.poll", "count", len(issues))
	metrics.GitHubIssuesImported(len(issues))
	f.syncIssuesToTasks(issues)
	f.syncLabeledIssuesToTasks()
	return IssuesPollInterval
}

func (f *IssuesFetcher) pollSnapshot() {
	repos := f.allowedRepos()
	snapshot, err := f.fetchSnapshot(repos, synapseIssueLabel)
	metrics.GitHubFetch(err == nil)
	if err != nil {
		if github.IsTransientError(err) {
			f.transientFetchFails++
			if f.transientFetchFails < issuesTransientWarnThreshold {
				f.logger.Info("issues.fetch", "err", err)
			} else {
				f.logger.Warn("issues.fetch", "err", err, "consecutive", f.transientFetchFails)
			}
		} else {
			f.transientFetchFails = 0
			f.logger.Warn("issues.fetch", "err", err)
		}
		return
	}
	f.transientFetchFails = 0

	f.emit("issues:updated", snapshot.Assigned)
	f.logger.Debug("issues.poll", "count", len(snapshot.Assigned))
	metrics.GitHubIssuesImported(len(snapshot.Assigned))
	f.syncIssuesToTasks(snapshot.Assigned)

	f.logger.Debug("labeled-issues.poll", "count", len(snapshot.Labeled))
	f.syncIssuesToTasks(snapshot.Labeled)
}

// syncLabeledIssuesToTasks fetches issues labeled 'sybra' across all registered
// pet projects and creates tasks for any not yet tracked.
func (f *IssuesFetcher) syncLabeledIssuesToTasks() {
	projects, err := f.projects.List()
	if err != nil {
		f.logger.Error("labeled-issues.list-projects", "err", err)
		return
	}

	repos := f.allowedReposFrom(projects)
	if len(repos) == 0 {
		return
	}

	labeled, err := f.fetchLabeled(repos, synapseIssueLabel)
	if err != nil {
		if github.IsTransientError(err) {
			f.transientLabeledFails++
			if f.transientLabeledFails < issuesTransientWarnThreshold {
				f.logger.Info("labeled-issues.fetch", "err", err)
			} else {
				f.logger.Warn("labeled-issues.fetch", "err", err, "consecutive", f.transientLabeledFails)
			}
		} else {
			f.transientLabeledFails = 0
			f.logger.Warn("labeled-issues.fetch", "err", err)
		}
		return
	}
	f.transientLabeledFails = 0
	f.logger.Debug("labeled-issues.poll", "count", len(labeled))
	f.syncIssuesToTasks(labeled)
}

func (f *IssuesFetcher) allowedRepos() []string {
	projects, err := f.projects.List()
	if err != nil {
		f.logger.Error("labeled-issues.list-projects", "err", err)
		return nil
	}
	return f.allowedReposFrom(projects)
}

func (f *IssuesFetcher) allowedReposFrom(projects []project.Project) []string {
	var repos []string
	for i := range projects {
		if f.allowsType(projects[i].Type) {
			repos = append(repos, projects[i].ID)
		}
	}
	return repos
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

		// Require the issue's repo to be a registered project. Issues from
		// unregistered repos are dropped entirely — sybra only tracks work
		// for repos the user has explicitly added as projects.
		proj, err := f.projects.Get(issue.Repository)
		if err != nil {
			continue
		}
		if !f.allowsType(proj.Type) {
			continue
		}

		// Task already exists with the issue URL as title (manually created).
		// Enrich it with the real title and link instead of creating a duplicate.
		if taskID, exists := urlTitleTasks[issue.URL]; exists {
			u := task.Update{
				Title:     task.Ptr(issue.Title),
				Issue:     task.Ptr(issue.URL),
				ProjectID: task.Ptr(issue.Repository),
			}
			if issue.Body != "" {
				u.Body = task.Ptr(issue.Body)
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
			Issue:     task.Ptr(issue.URL),
			Status:    task.Ptr(task.StatusTodo),
			ProjectID: task.Ptr(issue.Repository),
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
