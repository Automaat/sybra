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
	"github.com/Automaat/sybra/internal/umbrella"
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
	fetchIssueLinkedPRs   func(repo string, issueNumber int) ([]github.PullRequest, error)
	viewerLogin           func() string
	transientFetchFails   int
	transientLabeledFails int
	authCircuit           *AuthCircuit
	// umbrellaExpand, when set, auto-expands a detected ☂️ umbrella issue into a
	// gated task DAG instead of creating a flat task. nil = feature disabled.
	umbrellaExpand func(issueURL string) (umbrella.Result, error)
	// umbrellaCooldown holds the earliest next-attempt time per umbrella URL
	// after an expansion failure, so a broken umbrella does not re-run the
	// planner every poll.
	umbrellaCooldown map[string]time.Time
	// pollInterval overrides IssuesPollInterval when > 0 (set from config).
	pollInterval time.Duration
}

// SetPollInterval overrides the fixed issues poll cadence. Zero keeps the
// package default.
func (f *IssuesFetcher) SetPollInterval(d time.Duration) {
	f.pollInterval = d
}

func (f *IssuesFetcher) interval() time.Duration {
	base := f.pollInterval
	if base <= 0 {
		base = IssuesPollInterval
	}
	return github.ScaleInterval(base)
}

// umbrellaRetryCooldown is how long to wait before re-attempting an umbrella
// whose expansion failed.
const umbrellaRetryCooldown = time.Hour

// SetUmbrellaExpander enables auto-expansion of umbrella issues. fn typically
// wraps umbrella.Expand bound to the task store and a planner runner. A nil fn
// leaves the feature disabled.
func (f *IssuesFetcher) SetUmbrellaExpander(fn func(issueURL string) (umbrella.Result, error)) {
	f.umbrellaExpand = fn
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
		tasks:               tasks,
		projects:            projects,
		emit:                emit,
		logger:              logger,
		allowsType:          allowsType,
		fetchAssigned:       github.FetchAssignedIssues,
		fetchLabeled:        github.FetchLabeledIssuesForRepos,
		fetchSnapshot:       github.FetchIssueSnapshot,
		fetchIssueLinkedPRs: github.FetchIssueLinkedPRs,
		viewerLogin:         github.ViewerLogin,
		umbrellaCooldown:    map[string]time.Time{},
		authCircuit:         NewAuthCircuit("issues", logger),
	}
}

func (f *IssuesFetcher) Name() string { return "issues" }

// AuthCircuitOpen reports whether repeated GitHub auth failures have
// tripped this poller's circuit breaker (see poll.AuthCircuit).
func (f *IssuesFetcher) AuthCircuitOpen() bool { return f.authCircuit.Open() }

func (f *IssuesFetcher) Poll(ctx context.Context) time.Duration {
	if f.fetchSnapshot != nil {
		f.pollSnapshot(ctx)
		if f.authCircuit.Open() {
			return AuthCircuitBackoff
		}
		return f.interval()
	}

	issues, err := f.fetchAssigned()
	metrics.GitHubFetch(ctx, err == nil)
	if err != nil {
		switch {
		case github.IsAuthError(err):
			f.transientFetchFails = 0
			f.authCircuit.RecordFailure(err)
			if f.authCircuit.Open() {
				return AuthCircuitBackoff
			}
			// Pre-trip: Info, not Warn, so up-to-threshold auth failures don't
			// flood before the circuit's single trip line.
			f.logger.Info("issues.fetch", "err", err)
		case github.IsTransientError(err):
			f.transientFetchFails++
			if f.transientFetchFails < issuesTransientWarnThreshold {
				f.logger.Info("issues.fetch", "err", err)
			} else {
				f.logger.Warn("issues.fetch", "err", err, "consecutive", f.transientFetchFails)
			}
		default:
			f.transientFetchFails = 0
			f.logger.Warn("issues.fetch", "err", err)
		}
		return f.interval()
	}
	f.transientFetchFails = 0
	f.authCircuit.RecordSuccess()
	f.emit("issues:updated", issues)
	f.logger.Debug("issues.poll", "count", len(issues))
	metrics.GitHubIssuesImported(ctx, len(issues))
	f.syncIssuesToTasks(issues)
	f.syncLabeledIssuesToTasks()
	return f.interval()
}

func (f *IssuesFetcher) pollSnapshot(ctx context.Context) {
	repos := f.allowedRepos()
	snapshot, err := f.fetchSnapshot(repos, synapseIssueLabel)
	metrics.GitHubFetch(ctx, err == nil)
	if err != nil {
		switch {
		case github.IsAuthError(err):
			f.transientFetchFails = 0
			f.authCircuit.RecordFailure(err)
			if !f.authCircuit.Open() {
				// Pre-trip: Info, not Warn (see Poll's auth branch).
				f.logger.Info("issues.fetch", "err", err)
			}
		case github.IsTransientError(err):
			f.transientFetchFails++
			if f.transientFetchFails < issuesTransientWarnThreshold {
				f.logger.Info("issues.fetch", "err", err)
			} else {
				f.logger.Warn("issues.fetch", "err", err, "consecutive", f.transientFetchFails)
			}
		default:
			f.transientFetchFails = 0
			f.logger.Warn("issues.fetch", "err", err)
		}
		return
	}
	f.transientFetchFails = 0
	f.authCircuit.RecordSuccess()

	f.emit("issues:updated", snapshot.Assigned)
	f.logger.Debug("issues.poll", "count", len(snapshot.Assigned))
	metrics.GitHubIssuesImported(ctx, len(snapshot.Assigned))
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
	// Pass 1: expand umbrella issues first. Each creates gated child tasks, so
	// they must be persisted before the flat-task dedup snapshot is taken —
	// otherwise a sub-issue sharing this batch with its umbrella would slip
	// past dedup and get a duplicate, ungated flat task.
	flat := make([]github.Issue, 0, len(issues))
	for i := range issues {
		issue := &issues[i]
		if f.umbrellaExpand != nil && umbrella.IsUmbrellaIssue(issue.Title, issue.Labels) {
			f.expandUmbrellaIssue(issue)
			continue
		}
		flat = append(flat, *issue)
	}
	if len(flat) == 0 {
		return
	}

	// Pass 2: flat tasks, with a dedup snapshot taken AFTER expansion so the
	// children created above are visible.
	tasks, err := f.tasks.List()
	if err != nil {
		f.logger.Error("issue-sync.list-tasks", "err", err)
		return
	}
	issueURLs := make(map[string]struct{})
	urlTitleTasks := make(map[string]string) // issue URL → task ID for URL-titled stubs
	for i := range tasks {
		if tasks[i].Issue != "" {
			issueURLs[tasks[i].Issue] = struct{}{}
		}
		if strings.HasPrefix(tasks[i].Title, "https://github.com/") {
			urlTitleTasks[tasks[i].Title] = tasks[i].ID
		}
	}
	for i := range flat {
		f.syncFlatIssue(&flat[i], issueURLs, urlTitleTasks)
	}
}

// expandUmbrellaIssue auto-expands a detected umbrella issue, gated by the
// project-type allowlist and a per-umbrella failure cooldown so a broken
// umbrella does not re-run the planner every poll.
func (f *IssuesFetcher) expandUmbrellaIssue(issue *github.Issue) {
	proj, err := f.projects.Get(issue.Repository)
	if err != nil || !f.allowsType(proj.Type) {
		return
	}
	if until, ok := f.umbrellaCooldown[issue.URL]; ok && time.Now().Before(until) {
		return
	}
	res, err := f.umbrellaExpand(issue.URL)
	if err != nil {
		f.umbrellaCooldown[issue.URL] = time.Now().Add(umbrellaRetryCooldown)
		f.logger.Error("issue-sync.umbrella-expand", "issue", issue.URL, "err", err)
		return
	}
	delete(f.umbrellaCooldown, issue.URL)
	if res.Created > 0 {
		f.logger.Info("issue-sync.umbrella-expanded", "issue", issue.URL, "created", res.Created)
	}
	if res.Degraded {
		f.logger.Warn("issue-sync.umbrella-degraded", "issue", issue.URL, "created", res.Created)
	}
}

// syncFlatIssue creates or enriches a single non-umbrella issue task, honoring
// the dedup snapshot and project-type filter.
func (f *IssuesFetcher) syncFlatIssue(issue *github.Issue, issueURLs map[string]struct{}, urlTitleTasks map[string]string) {
	if _, exists := issueURLs[issue.URL]; exists {
		return
	}

	// Require the issue's repo to be a registered project. Issues from
	// unregistered repos are dropped entirely — sybra only tracks work
	// for repos the user has explicitly added as projects.
	proj, err := f.projects.Get(issue.Repository)
	if err != nil || !f.allowsType(proj.Type) {
		return
	}

	// Task already exists with the issue URL as title (manually created).
	// Enrich it with the real title and link instead of creating a duplicate.
	if taskID, exists := urlTitleTasks[issue.URL]; exists {
		u := task.Update{
			Title:     task.Ptr(issue.Title),
			Issue:     task.Ptr(issue.URL),
			ProjectID: task.Ptr(issue.Repository),
		}
		f.enrichLinkedViewerPR(issue, &u)
		if issue.Body != "" {
			u.Body = task.Ptr(issue.Body)
		}
		if _, err := f.tasks.Update(taskID, u); err != nil {
			f.logger.Error("issue-sync.enrich", "task_id", taskID, "err", err)
		} else {
			f.logger.Info("issue-sync.enriched", "task_id", taskID, "issue", issue.URL, "title", issue.Title)
		}
		return
	}

	t, err := f.tasks.Create(issue.Title, issue.Body, "headless")
	if err != nil {
		f.logger.Error("issue-sync.create", "issue", issue.URL, "err", err)
		return
	}

	u := task.Update{
		Issue:     task.Ptr(issue.URL),
		Status:    task.Ptr(task.StatusTodo),
		ProjectID: task.Ptr(issue.Repository),
	}
	f.enrichLinkedViewerPR(issue, &u)
	if len(issue.Labels) > 0 {
		labels := issue.Labels
		u.Tags = &labels
	}
	if _, err := f.tasks.Update(t.ID, u); err != nil {
		f.logger.Error("issue-sync.update", "task_id", t.ID, "err", err)
	}
	f.logger.Info("issue-sync.created", "task_id", t.ID, "issue", issue.URL)
}

func (f *IssuesFetcher) enrichLinkedViewerPR(issue *github.Issue, u *task.Update) {
	if f.fetchIssueLinkedPRs == nil || f.viewerLogin == nil {
		return
	}
	prs, err := f.fetchIssueLinkedPRs(issue.Repository, issue.Number)
	if err != nil {
		f.logger.Warn("issue-sync.linked-prs", "issue", issue.URL, "err", err)
		return
	}
	if len(prs) == 0 {
		return
	}
	viewer := f.viewerLogin()
	if viewer == "" {
		return
	}
	var mine []github.PullRequest
	for i := range prs {
		if strings.EqualFold(prs[i].Author, viewer) {
			mine = append(mine, prs[i])
		}
	}
	if len(mine) != 1 {
		if len(mine) > 1 {
			f.logger.Warn("issue-sync.linked-prs.ambiguous", "issue", issue.URL, "count", len(mine))
		}
		return
	}
	u.PRNumber = task.Ptr(mine[0].Number)
	u.Branch = task.Ptr(mine[0].HeadRefName)
	u.Status = task.Ptr(task.StatusInReview)
}
