package sybra

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/todoist"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// IntegrationService exposes Todoist, Renovate, and GitHub issue operations
// as Wails-bound methods.
type IntegrationService struct {
	tasks           *task.Manager
	projects        *project.Store
	agents          *agent.Manager
	worktrees       *worktree.Manager
	audit           *audit.Logger
	cfg             *config.Config
	logger          *slog.Logger
	todoistHandler  *poll.TodoistHandler
	renovateHandler *poll.RenovateHandler
	workflowEngine  *workflow.Engine
	providerHealth  *provider.Checker
	saveConfig      func() error
}

// SyncTodoist triggers an immediate Todoist sync.
func (s *IntegrationService) SyncTodoist() error {
	if s.todoistHandler == nil {
		return fmt.Errorf("todoist integration not enabled")
	}
	s.todoistHandler.PollAndSync()
	return nil
}

// GetTodoistProjects returns Todoist projects for the settings UI.
func (s *IntegrationService) GetTodoistProjects() ([]todoist.Project, error) {
	token := s.cfg.Todoist.APIToken
	if token == "" {
		return nil, fmt.Errorf("todoist API token not configured")
	}
	c := todoist.NewClient(token)
	return c.ListProjects()
}

// TodoistEnabled returns whether the todoist handler is active.
func (s *IntegrationService) TodoistEnabled() bool {
	return s.todoistHandler != nil
}

// FetchRenovatePRs returns Renovate PRs for manual refresh.
func (s *IntegrationService) FetchRenovatePRs() ([]github.RenovatePR, error) {
	if s.renovateHandler == nil {
		return nil, nil
	}
	repos := s.renovateHandler.Repos()
	if len(repos) == 0 {
		return nil, nil
	}
	return github.FetchRenovatePRs(s.cfg.Renovate.Author, repos)
}

// MergeRenovatePR merges a Renovate PR.
func (s *IntegrationService) MergeRenovatePR(repo string, number int) error {
	return github.MergePR(repo, number)
}

// ApproveRenovatePR approves a Renovate PR.
func (s *IntegrationService) ApproveRenovatePR(repo string, number int) error {
	return github.ApprovePR(repo, number)
}

// RerunRenovateChecks reruns failed CI checks on a Renovate PR.
func (s *IntegrationService) RerunRenovateChecks(repo string, number int) error {
	return github.RerunFailedChecks(repo, number)
}

// FixRenovateCI spawns an agent to fix CI failures on a Renovate PR.
func (s *IntegrationService) FixRenovateCI(repo string, number int, branch, title string) error {
	tasks, err := s.tasks.List()
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	for i := range tasks {
		t := &tasks[i]
		if t.PRNumber == number && t.ProjectID == repo && slices.Contains(t.Tags, "renovate-fix") {
			if !task.IsTerminalStatus(t.Status) && s.agents.HasRunningAgentForTask(t.ID) {
				return nil // already being fixed
			}
		}
	}

	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	t, err := s.tasks.Create("Fix CI: "+title, prURL, "headless")
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	tags := []string{"renovate-fix"}
	if _, err := s.tasks.Update(t.ID, task.Update{
		ProjectID: task.Ptr(repo),
		PRNumber:  task.Ptr(number),
		Tags:      &tags,
		RunRole:   task.Ptr("pr-fix"),
	}); err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	t, _ = s.tasks.Get(t.ID)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := s.worktrees.PrepareForFix(t, number)
		if wtErr != nil {
			s.logger.Error("renovate-fix.worktree", "task_id", t.ID, "err", wtErr)
			return fmt.Errorf("prepare worktree: %w", wtErr)
		}
		dir = d
	}

	if s.workflowEngine == nil {
		return fmt.Errorf("workflow engine not available")
	}

	prompt := fmt.Sprintf(
		"# Task: Fix CI: %s\n\n"+
			"Fix failing CI on branch `%s` (PR #%d). "+
			"Check the failing run with `gh run view --log-failed`, "+
			"fix the code, commit and push. No unrelated changes.",
		title, branch, number,
	)

	ciFailure := string(github.PRIssueCIFailure)
	vars := map[string]string{
		"prompt":                prompt,
		"pr_issue_kind":         ciFailure,
		workflow.WorkflowVarDir: dir,
	}
	wfID, err := s.workflowEngine.DispatchEvent(t.ID, "pr.event",
		map[string]string{"pr.issue_kind": ciFailure}, vars)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			s.logger.Info("renovate-fix.workflow-already-active", "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("dispatch pr.event: %w", err)
	}
	if wfID == "" {
		return fmt.Errorf("no workflow matched pr.event for %s", ciFailure)
	}

	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventRenovateCIFix,
			TaskID: t.ID,
			Data:   map[string]any{"pr": number, "repo": repo},
		})
	}

	s.logger.Info("renovate-fix.started", "task_id", t.ID, "pr", number)
	return nil
}

// FetchAssignedIssues returns GitHub issues assigned to the current user.
func (s *IntegrationService) FetchAssignedIssues() ([]github.Issue, error) {
	return github.FetchAssignedIssues()
}
