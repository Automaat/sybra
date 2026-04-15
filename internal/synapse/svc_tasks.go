package synapse

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
	"github.com/Automaat/synapse/internal/sandbox"
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
	sandboxes      *sandbox.Manager
	wg             *sync.WaitGroup
	logger         *slog.Logger
	audit          *audit.Logger
}

// ListTasks returns all tasks from the store, excluding ephemeral chat tasks.
// Chat tasks are surfaced exclusively through the Chats view.
func (s *TaskService) ListTasks() ([]task.Task, error) {
	all, err := s.tasks.List()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if all[i].TaskType == task.TaskTypeChat {
			continue
		}
		out = append(out, all[i])
	}
	return out, nil
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
//
// Moving a task to "testing" is refused if another workflow is still active —
// the testing workflow needs a clean slate (no in-flight agents or pending
// human steps) so the user can't accidentally lose context by dragging.
//
// Moving a task to "in-progress" when its workflow is terminal (completed or
// failed) and no agent is running restarts the workflow — allowing the user to
// retry implementation after a human-required escalation.
func (s *TaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	cur, _ := s.tasks.Get(id)

	if status, ok := updates["status"].(string); ok {
		// Reject status regressions while an agent is running on this task.
		// Moving back to todo/new/done while an agent is active loses in-flight work.
		agentBlockedStatuses := map[string]bool{
			string(task.StatusNew):  true,
			string(task.StatusTodo): true,
			string(task.StatusDone): true,
		}
		if agentBlockedStatuses[status] && s.agents.HasRunningAgentForTask(id) {
			return cur, fmt.Errorf("cannot move to %q: stop the running agent first", status)
		}

		if status == string(task.StatusTesting) {
			if cur.Workflow != nil &&
				cur.Workflow.State != workflow.ExecCompleted &&
				cur.Workflow.State != workflow.ExecFailed {
				return cur, fmt.Errorf("cannot move to testing: task has active workflow %q (state=%s)",
					cur.Workflow.WorkflowID, cur.Workflow.State)
			}
		}
	}
	t, err := s.tasks.UpdateMap(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusDone {
		s.wg.Go(func() {
			s.worktrees.Remove(t.ID)
			if s.sandboxes != nil {
				s.sandboxes.Stop(t.ID)
			}
		})
	}

	// When manually moved to in-progress with a terminal workflow and no live
	// agent, restart the workflow so the user doesn't need to manually dispatch.
	if s.workflowEngine != nil {
		if newStatus, ok := updates["status"].(string); ok &&
			newStatus == string(task.StatusInProgress) &&
			cur.Workflow != nil &&
			(cur.Workflow.State == workflow.ExecCompleted || cur.Workflow.State == workflow.ExecFailed) &&
			!s.agents.HasRunningAgentForTask(id) {
			wfID := cur.Workflow.WorkflowID
			s.logger.Info("workflow.restart", "task_id", id, "workflow", wfID)
			s.wg.Go(func() {
				if wfErr := s.workflowEngine.StartWorkflow(id, wfID); wfErr != nil {
					s.logger.Error("workflow.restart.failed", "task_id", id, "err", wfErr)
				}
			})
		}
	}

	return t, nil
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (s *TaskService) DeleteTask(id string) error {
	s.logger.Info("task.delete", "task_id", id)
	s.agents.KillAgentsForTask(id, 10*time.Second)
	if s.sandboxes != nil {
		s.sandboxes.Stop(id)
	}
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

	slug := task.Slugify(pr.Title)
	u := task.Update{
		Title:     task.Ptr(pr.Title),
		ProjectID: task.Ptr(repo),
		PRNumber:  task.Ptr(pr.Number),
		Branch:    task.Ptr(pr.HeadRefName),
		Slug:      task.Ptr(slug),
	}
	var labels []string
	if len(pr.Labels) > 0 {
		labels = pr.Labels
		u.Tags = &labels
	}

	isMyPR := viewer != "" && strings.EqualFold(pr.Author, viewer)
	if isMyPR {
		u.Status = task.Ptr(task.StatusInReview)
		if _, err := s.tasks.Update(taskID, u); err != nil {
			s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
			return
		}
		s.logger.Info("enrich-pr.my-pr", "task_id", taskID, "pr", number, "title", pr.Title)
		return
	}

	// Not my PR: add review tag and start review agent.
	labels = append(labels, "review")
	u.Tags = &labels
	if _, err := s.tasks.Update(taskID, u); err != nil {
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
	if _, err := s.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
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
	slug := task.Slugify(issue.Title)
	u := task.Update{
		Title:     task.Ptr(issue.Title),
		Issue:     task.Ptr(issue.URL),
		ProjectID: task.Ptr(repo),
		Slug:      task.Ptr(slug),
	}
	if issue.Body != "" {
		u.Body = task.Ptr(issue.Body)
	}
	if len(issue.Labels) > 0 {
		labels := issue.Labels
		u.Tags = &labels
	}
	if _, err := s.tasks.Update(taskID, u); err != nil {
		s.logger.Error("enrich-issue.update", "task_id", taskID, "err", err)
		return
	}
	s.logger.Info("enrich-issue.done", "task_id", taskID, "title", issue.Title)
}
