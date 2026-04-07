package main

import (
	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
)

// Compile-time interface checks.
var (
	_ workflow.TaskProvider  = (*taskAdapter)(nil)
	_ workflow.AgentLauncher = (*agentAdapter)(nil)
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

func (a *taskAdapter) UpdateTaskStatus(id, status, reason string) error {
	updates := map[string]any{"status": status}
	if reason != "" {
		updates["status_reason"] = reason
	}
	_, err := a.tasks.Update(id, updates)
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.Update(id, map[string]any{"workflow": wf})
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:        t.ID,
		Title:     t.Title,
		Status:    string(t.Status),
		Tags:      t.Tags,
		AgentMode: t.AgentMode,
		ProjectID: t.ProjectID,
		PRNumber:  t.PRNumber,
		Branch:    t.Branch,
		Body:      t.Body,
		Workflow:  t.Workflow,
	}
}

// agentAdapter bridges agent.Manager + AgentOrchestrator → workflow.AgentLauncher.
type agentAdapter struct {
	agents    *agent.Manager
	agentOrch *AgentOrchestrator
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, prompt string, allowedTools []string) (string, error) {
	// For implementation agents, use the full orchestrator (handles worktree, project assignment).
	if role == "" || role == string(agent.RoleImplementation) {
		ag, err := a.agentOrch.StartAgent(taskID, mode, prompt)
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
	}

	// Plan and pr-fix agents need worktree.
	if r == agent.RolePlan || r == agent.RolePRFix {
		t = a.agentOrch.autoAssignProject(t)
		if t.ProjectID != "" {
			dir, wtErr := a.agentOrch.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return "", wtErr
			}
			cfg.Dir = dir
		}
	}

	ag, err := a.agents.Run(cfg)
	if err != nil {
		return "", err
	}
	return ag.ID, nil
}

func (a *agentAdapter) HasRunningAgent(taskID string) bool {
	return a.agents.HasRunningAgentForTask(taskID)
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
