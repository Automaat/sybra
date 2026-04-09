package main

import (
	"log/slog"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
)

// Compile-time interface checks.
var (
	_ workflow.TaskProvider  = (*taskAdapter)(nil)
	_ workflow.AgentLauncher = (*agentAdapter)(nil)
	_ workflow.PRLinker      = (*prLinkerAdapter)(nil)
)

// taskAdapter bridges task.Manager → workflow.TaskProvider.
type taskAdapter struct {
	tasks *task.Manager
}

func (a *taskAdapter) GetTask(id string) (workflow.TaskInfo, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return workflow.TaskInfo{}, err
	}
	return taskToInfo(t), nil
}

func (a *taskAdapter) ListTasks() ([]workflow.TaskInfo, error) {
	tasks, err := a.tasks.List()
	if err != nil {
		return nil, err
	}
	infos := make([]workflow.TaskInfo, len(tasks))
	for i := range tasks {
		infos[i] = taskToInfo(tasks[i])
	}
	return infos, nil
}

func (a *taskAdapter) UpdateTaskStatus(id, status, reason string) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	u := task.Update{Status: &st}
	if reason != "" {
		u.StatusReason = &reason
	}
	_, err = a.tasks.Update(id, u)
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.Update(id, task.Update{Workflow: &wf})
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:           t.ID,
		Title:        t.Title,
		Status:       string(t.Status),
		Tags:         t.Tags,
		AgentMode:    t.AgentMode,
		ProjectID:    t.ProjectID,
		PRNumber:     t.PRNumber,
		Branch:       t.Branch,
		Body:         t.Body,
		Plan:         t.Plan,
		PlanCritique: t.PlanCritique,
		Issue:        t.Issue,
		Workflow:     t.Workflow,
	}
}

// prLinkerAdapter wires the workflow engine's PRLinker interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type prLinkerAdapter struct{}

func (prLinkerAdapter) GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error) {
	return github.FetchPRClosingIssues(repo, prNumber)
}

func (prLinkerAdapter) EditBody(repo string, prNumber int, body string) error {
	return github.EditPRBody(repo, prNumber, body)
}

// agentAdapter bridges agent.Manager + AgentOrchestrator → workflow.AgentLauncher.
type agentAdapter struct {
	agents    *agent.Manager
	agentOrch *AgentOrchestrator
	tasks     *task.Manager
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool) (string, error) {
	// For implementation agents, use the full orchestrator (handles worktree, project assignment).
	if role == "" || role == string(agent.RoleImplementation) {
		ag, err := a.agentOrch.StartAgent(taskID, mode, prompt, oneShot)
		if err != nil {
			return "", err
		}
		return ag.ID, nil
	}

	// For system agents (triage, eval, plan, etc.), build RunConfig directly.
	r := agent.Role(role)
	t, err := a.agentOrch.tasks.Get(taskID)
	if err != nil {
		return "", err
	}

	cfg := agent.RunConfig{
		TaskID:       taskID,
		Name:         r.AgentName(t.Title),
		Mode:         mode,
		Prompt:       prompt,
		AllowedTools: allowedTools,
		Model:        model,
		Dir:          dir,
		OneShot:      oneShot,
	}

	// Caller-provided dir takes precedence (e.g. pr-fix flow pre-stages a
	// worktree via PrepareForFix). Only fall back to PrepareForTask when no
	// dir is provided and the step declared needs_worktree.
	if cfg.Dir == "" && needsWorktree {
		t = a.agentOrch.autoAssignProject(t)
		if t.ProjectID != "" {
			d, wtErr := a.agentOrch.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return "", wtErr
			}
			cfg.Dir = d
		}
	}

	ag, err := a.agents.Run(cfg)
	if err != nil {
		return "", err
	}

	// Record agent run on task (was missing for system roles).
	if addErr := a.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      role,
		Mode:      mode,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); addErr != nil {
		slog.Error("agent-adapter.add-run", "task_id", taskID, "agent_id", ag.ID, "err", addErr)
	}

	return ag.ID, nil
}

func (a *agentAdapter) HasRunningAgent(taskID string) bool {
	return a.agents.HasRunningAgentForTask(taskID)
}

func (a *agentAdapter) FindRunningAgentForRole(taskID, role string) (string, bool) {
	r := agent.Role(role)
	ag := a.agents.FindRunningAgentForTask(taskID, r)
	if ag == nil {
		return "", false
	}
	return ag.ID, true
}

func (a *agentAdapter) StopAgentsForTask(taskID, role string) {
	r := agent.Role(role)
	for _, ag := range a.agents.FindAllRunningAgentsForTask(taskID, r) {
		_ = a.agents.StopAgent(ag.ID)
	}
}

func (a *agentAdapter) SendPrompt(agentID, message string) error {
	return a.agents.SendPromptToAgent(agentID, message)
}
