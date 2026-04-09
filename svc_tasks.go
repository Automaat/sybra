package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
)

// TaskService exposes task CRUD operations as Wails-bound methods.
type TaskService struct {
	tasks          *task.Manager
	agents         *agent.Manager
	workflowEngine *workflow.Engine
	worktrees      *worktree.Manager
	wg             *sync.WaitGroup
	logger         *slog.Logger
	audit          *audit.Logger
}

// ListTasks returns all tasks from the store.
func (s *TaskService) ListTasks() ([]task.Task, error) {
	return s.tasks.List()
}

// GetTask returns a single task by ID.
func (s *TaskService) GetTask(id string) (task.Task, error) {
	return s.tasks.Get(id)
}

// CreateTask creates a new task and starts a matching workflow.
// If the title is a GitHub issue URL, fetches real title/body from GitHub.
func (s *TaskService) CreateTask(title, body, mode string) (task.Task, error) {
	t, err := s.tasks.Create(title, body, mode)
	if err != nil {
		return t, err
	}
	// Enrich from GitHub PR URL if title looks like one.
	if repo, number := github.ParsePRURL(title); repo != "" {
		s.wg.Go(func() {
			s.enrichFromPR(t.ID, repo, number)
		})
	} else if repo, number := github.ParseIssueURL(title); repo != "" {
		// Enrich from GitHub issue URL if title looks like one.
		s.wg.Go(func() {
			s.enrichFromIssue(t.ID, repo, number)
		})
	}
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskCreated,
			TaskID: t.ID,
			Data:   map[string]any{"title": title, "mode": mode},
		})
	}
	// Match and start a workflow for the new task.
	if s.workflowEngine != nil && t.Status == task.StatusTodo {
		info := taskToInfo(t)
		if def := s.workflowEngine.MatchWorkflow(info, "task.created"); def != nil {
			s.logger.Info("workflow.auto-start", "task_id", t.ID, "workflow", def.ID)
			s.wg.Go(func() {
				if wfErr := s.workflowEngine.StartWorkflow(t.ID, def.ID); wfErr != nil {
					s.logger.Error("workflow.auto-start.failed", "task_id", t.ID, "err", wfErr)
				}
			})
		}
	}
	return t, nil
}

// UpdateTask applies field updates to a task. The workflow engine drives
// all status-based transitions; this method only handles cleanup on done.
func (s *TaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	t, err := s.tasks.Update(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusDone {
		s.wg.Go(func() { s.worktrees.Remove(t.ID) })
	}
	return t, nil
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (s *TaskService) DeleteTask(id string) error {
	s.logger.Info("task.delete", "task_id", id)
	s.agents.KillAgentsForTask(id, 10*time.Second)
	s.worktrees.Remove(id)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskDeleted,
			TaskID: id,
		})
	}
	if err := s.tasks.Delete(id); err != nil {
		s.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return err
	}
	return nil
}

// enrichFromPR fetches a GitHub PR and updates the task.
// If the PR was authored by the current viewer, moves to in-review for PR monitoring.
// Otherwise, starts a headless review agent with /staff-code-review.
func (s *TaskService) enrichFromPR(taskID, repo string, number int) {
	pr, err := github.FetchPR(repo, number)
	if err != nil {
		s.logger.Error("enrich-pr.fetch", "task_id", taskID, "repo", repo, "number", number, "err", err)
		return
	}
	viewer := github.ViewerLogin()

	updates := map[string]any{
		"title":      pr.Title,
		"project_id": repo,
		"pr_number":  pr.Number,
		"branch":     pr.HeadRefName,
		"slug":       task.Slugify(pr.Title),
	}
	if len(pr.Labels) > 0 {
		updates["tags"] = pr.Labels
	}

	isMyPR := viewer != "" && strings.EqualFold(pr.Author, viewer)
	if isMyPR {
		updates["status"] = string(task.StatusInReview)
		if _, err := s.tasks.Update(taskID, updates); err != nil {
			s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
			return
		}
		s.logger.Info("enrich-pr.my-pr", "task_id", taskID, "pr", number, "title", pr.Title)
		return
	}

	// Not my PR: add review tag and start review agent.
	tags, _ := updates["tags"].([]string)
	updates["tags"] = append(tags, "review")
	if _, err := s.tasks.Update(taskID, updates); err != nil {
		s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
		return
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		s.logger.Error("enrich-pr.get", "task_id", taskID, "err", err)
		return
	}
	if err := s.startPRReviewAgent(t); err != nil {
		s.logger.Error("enrich-pr.review-agent", "task_id", taskID, "err", err)
	}
}

// startPRReviewAgent starts a headless agent that runs /staff-code-review on the PR.
func (s *TaskService) startPRReviewAgent(t task.Task) error {
	dir := config.HomeDir()
	if t.ProjectID != "" {
		d, err := s.worktrees.PrepareForReview(t)
		if err != nil {
			s.logger.Warn("enrich-pr.worktree", "task_id", t.ID, "err", err)
		} else {
			dir = d
		}
	}

	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)
	ag, err := s.agents.Run(agent.RunConfig{
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
	if err := s.tasks.AddRun(t.ID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      string(agent.RoleReview),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		s.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	if _, err := s.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
		s.logger.Error("enrich-pr.status", "task_id", t.ID, "err", err)
	}
	s.logger.Info("enrich-pr.review-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

// enrichFromIssue fetches a GitHub issue and updates the task with real title/body.
func (s *TaskService) enrichFromIssue(taskID, repo string, number int) {
	issue, err := github.FetchIssue(repo, number)
	if err != nil {
		s.logger.Error("enrich-issue.fetch", "task_id", taskID, "repo", repo, "number", number, "err", err)
		return
	}
	updates := map[string]any{
		"title":      issue.Title,
		"issue":      issue.URL,
		"project_id": repo,
		"slug":       task.Slugify(issue.Title),
	}
	if issue.Body != "" {
		updates["body"] = issue.Body
	}
	if len(issue.Labels) > 0 {
		updates["tags"] = issue.Labels
	}
	if _, err := s.tasks.Update(taskID, updates); err != nil {
		s.logger.Error("enrich-issue.update", "task_id", taskID, "err", err)
		return
	}
	s.logger.Info("enrich-issue.done", "task_id", taskID, "title", issue.Title)
}
