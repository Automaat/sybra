package synapse

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/worktree"
)

// resolveExecution derives the effective mode, directory, permission mode, and
// whether project worktree setup should be skipped based on the task's type.
// hintMode is used when the task type does not force a specific mode.
// Permission priority: task-level override > TaskType hardcoded > config default > true.
func resolveExecution(t task.Task, hintMode, researchMachineDir string, cfg *config.Config) (mode, dir string, requirePerm, skipWorktree bool) {
	switch t.TaskType {
	case task.TaskTypeDebug:
		return "interactive", "", true, false
	case task.TaskTypeResearch:
		return "headless", researchMachineDir, resolvePermission(t, cfg), true
	default:
		return hintMode, "", resolvePermission(t, cfg), false
	}
}

// resolvePermission returns the effective require_permissions value for a task.
// Priority: task field > config default > true (safe default).
func resolvePermission(t task.Task, cfg *config.Config) bool {
	if t.RequirePermissions != nil {
		return *t.RequirePermissions
	}
	return cfg.DefaultRequirePermissions()
}

// AgentOrchestrator manages agent lifecycle: worktree setup, project
// assignment, and agent launching for a task.
type AgentOrchestrator struct {
	DomainHandler
	tasks     *task.Manager
	projects  *project.Store
	agents    *agent.Manager
	worktrees *worktree.Manager
	cfg       *config.Config
}

func newAgentOrchestrator(
	tasks *task.Manager,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	worktrees *worktree.Manager,
	cfg *config.Config,
) *AgentOrchestrator {
	return &AgentOrchestrator{
		DomainHandler: DomainHandler{audit: al, logger: logger},
		tasks:         tasks,
		projects:      projects,
		agents:        agents,
		worktrees:     worktrees,
		cfg:           cfg,
	}
}

func (o *AgentOrchestrator) StartAgent(taskID, mode, prompt string, oneShot bool) (*agent.Agent, error) {
	t, err := o.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	researchDir := ""
	if o.cfg != nil {
		researchDir = o.cfg.Agent.ResearchMachineDir
	}
	effMode, dir, requirePerm, skipWT := resolveExecution(t, mode, researchDir, o.cfg)
	if !skipWT {
		t = o.autoAssignProject(t)
		if t.ProjectID == "" {
			return nil, fmt.Errorf("task %s has no project_id: refusing to start agent without isolated worktree", taskID)
		}
		d, wtErr := o.worktrees.PrepareForTask(t)
		if wtErr != nil {
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}
	if dir == "" {
		return nil, fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Synapse cwd", taskID, skipWT)
	}

	// Flip status only after worktree prep succeeds — otherwise a failed
	// prep leaves the task stuck at in-progress with no running agent,
	// which triggers restart-stale respawn loops on every app restart.
	if t.Status != task.StatusInProgress {
		if _, err := o.tasks.Update(taskID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
			o.logger.Error("task.auto-status", "task_id", taskID, "err", err)
		}
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               t.Title,
		Mode:               effMode,
		Prompt:             fullPrompt,
		AllowedTools:       t.AllowedTools,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
		OneShot:            oneShot,
	})
	if err != nil {
		return nil, err
	}
	skipPerm := !requirePerm && len(t.AllowedTools) == 0
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{
		"mode": effMode, "title": t.Title, "task_type": string(t.TaskType), "provider": ag.Provider,
		"allowed_tools": t.AllowedTools, "require_permissions": requirePerm, "skip_permissions": skipPerm,
	})
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Mode:      effMode,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

func (o *AgentOrchestrator) autoAssignProject(t task.Task) task.Task {
	if t.ProjectID != "" || o.projects == nil {
		return t
	}
	projects, err := o.projects.List()
	if err != nil || len(projects) != 1 {
		return t
	}
	t.ProjectID = projects[0].ID
	if _, err := o.tasks.Update(t.ID, task.Update{ProjectID: task.Ptr(t.ProjectID)}); err != nil {
		o.logger.Error("auto-assign-project", "task_id", t.ID, "err", err)
	} else {
		o.logger.Info("auto-assign-project", "task_id", t.ID, "project", t.ProjectID)
	}
	return t
}

// StartPRFixAgent starts a headless agent to address review comments on
// the task's PR. Named "pr-fix:" so handleAgentComplete routes it correctly.
func (o *AgentOrchestrator) StartPRFixAgent(taskID string) error {
	t, err := o.tasks.Get(taskID)
	if err != nil {
		return err
	}

	researchDir := ""
	if o.cfg != nil {
		researchDir = o.cfg.Agent.ResearchMachineDir
	}
	effMode, dir, requirePerm, skipWT := resolveExecution(t, t.AgentMode, researchDir, o.cfg)
	if !skipWT {
		t = o.autoAssignProject(t)
		if t.ProjectID == "" {
			return fmt.Errorf("task %s has no project_id: refusing to start pr-fix agent without isolated worktree", taskID)
		}
		d, wtErr := o.worktrees.PrepareForTask(t)
		if wtErr != nil {
			return fmt.Errorf("worktree required: %w", wtErr)
		}
		dir = d
	}
	if dir == "" {
		return fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Synapse cwd", taskID, skipWT)
	}

	prompt := buildPRFixPrompt(t, o.logger)
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               agent.RolePRFix.AgentName(t.Title),
		Mode:               effMode,
		Prompt:             prompt,
		AllowedTools:       t.AllowedTools,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
	})
	if err != nil {
		return err
	}

	skipPerm := !requirePerm && len(t.AllowedTools) == 0
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{
		"mode": effMode, "title": t.Title, "role": "pr-fix", "task_type": string(t.TaskType), "provider": ag.Provider,
		"allowed_tools": t.AllowedTools, "require_permissions": requirePerm, "skip_permissions": skipPerm,
	})
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: effMode,
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return nil
}

// buildPRFixPrompt constructs the prompt for a PR fix agent.
// If the task has an associated PR, it fetches review context (URL, branch,
// review comments) and includes it so the agent amends the existing PR rather
// than starting from scratch.
func buildPRFixPrompt(t task.Task, logger *slog.Logger) string {
	base := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\nFix the issues raised in the PR review. Push the changes when done.", t.Title, t.Body)
	if t.PRNumber == 0 || t.ProjectID == "" {
		return base
	}

	prCtx, err := github.FetchPRContext(t.ProjectID, t.PRNumber)
	if err != nil {
		logger.Warn("pr-fix.fetch-context", "pr", t.PRNumber, "err", err)
		// Fall back to minimal context from task fields.
		branch := t.Branch
		if branch == "" {
			branch = "unknown"
		}
		return fmt.Sprintf("%s\n\n## PR Context\n- PR: #%d (https://github.com/%s/pull/%d)\n- Branch: `%s`\n\nCheck out the branch and push amended commits to the same branch.", base, t.PRNumber, t.ProjectID, t.PRNumber, branch)
	}

	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n## PR Context\n")
	fmt.Fprintf(&sb, "- PR: #%d (%s)\n", t.PRNumber, prCtx.URL)
	fmt.Fprintf(&sb, "- Branch: `%s`\n", prCtx.Branch)
	sb.WriteString("\nDo NOT open a new PR. Push commits to the existing branch `")
	sb.WriteString(prCtx.Branch)
	sb.WriteString("`.\n")

	if len(prCtx.Comments) > 0 {
		sb.WriteString("\n## Review Comments to Address\n")
		for i, c := range prCtx.Comments {
			fmt.Fprintf(&sb, "\n### Comment %d", i+1)
			if c.Author != "" {
				fmt.Fprintf(&sb, " (by @%s)", c.Author)
			}
			if c.Path != "" {
				fmt.Fprintf(&sb, " on `%s`", c.Path)
			}
			sb.WriteString("\n")
			sb.WriteString(c.Body)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
