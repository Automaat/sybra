package synapse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/provider"
	"github.com/Automaat/synapse/internal/sandbox"
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
	case task.TaskTypeChat:
		return "interactive", "", resolvePermission(t, cfg), false
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

// pickImplementationResumeSession walks AgentRuns newest-first and returns
// the most recent session_id from a prior implementation run that belongs
// to the current workflow execution.
//
// Two filters are applied:
//
//  1. Role must be implementation. Other roles (triage, plan, eval, …) own
//     their own session state, often run in a different cwd, and resuming
//     them from the implementation worktree makes claude bail with
//     error_during_execution before the prompt is ever sent. Empty Role is
//     allowed only for legacy runs predating the orchestrator role-recording
//     fix; new runs always carry Role explicitly.
//  2. StartedAt must be at or after workflowStart. A previous workflow
//     execution may have left an aborted implementation run with a
//     session_id that no longer exists in claude's session store. Resuming
//     it would make claude exit with "No conversation found", cost $0,
//     and verify_commits flip the task to human-required without ever
//     running the implementation prompt. workflowStart=zero disables the
//     time filter (useful for callers that have no execution context).
func pickImplementationResumeSession(runs []task.AgentRun, workflowStart time.Time) string {
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if run.SessionID == "" {
			continue
		}
		if run.Role != "" && run.Role != string(agent.RoleImplementation) {
			continue
		}
		if !workflowStart.IsZero() && run.StartedAt.Before(workflowStart) {
			continue
		}
		return run.SessionID
	}
	return ""
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
	sandboxes *sandbox.Manager
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

	var workflowStart time.Time
	if t.Workflow != nil {
		workflowStart = t.Workflow.StartedAt
	}
	resumeSessionID := pickImplementationResumeSession(t.AgentRuns, workflowStart)

	var extraEnv []string
	if o.sandboxes != nil && t.ProjectID != "" {
		if proj, pErr := o.projects.Get(t.ProjectID); pErr == nil && proj.Sandbox != nil {
			inst := o.sandboxes.Get(taskID)
			if inst == nil {
				inst, err = o.sandboxes.Start(context.Background(), taskID, dir, proj.Sandbox)
				if err != nil {
					o.logger.Warn("sandbox.start.failed", "task_id", taskID, "err", err)
					err = nil // non-fatal: agent runs without sandbox
				}
			}
			if inst != nil {
				extraEnv = inst.EnvVars()
			}
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
		ResumeSessionID:    resumeSessionID,
		ExtraEnv:           extraEnv,
	})
	if err != nil {
		// Gate block leaves no running agent. Flip the task back to todo so
		// watchdog / restart-stale loops don't chase a ghost in-progress row.
		if errors.Is(err, provider.ErrProviderUnhealthy) {
			if _, rerr := o.tasks.Update(taskID, task.Update{Status: task.Ptr(task.StatusTodo)}); rerr != nil {
				o.logger.Error("task.revert-on-gate", "task_id", taskID, "err", rerr)
			}
			o.logAudit(audit.EventProviderGateBlocked, taskID, "", map[string]any{"err": err.Error()})
			o.logger.Info("agent.start.gated", "task_id", taskID, "err", err)
		}
		return nil, err
	}
	skipPerm := !requirePerm && len(t.AllowedTools) == 0
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{
		"mode": effMode, "title": t.Title, "task_type": string(t.TaskType), "provider": ag.Provider,
		"allowed_tools": t.AllowedTools, "require_permissions": requirePerm, "skip_permissions": skipPerm,
	})
	var nextStatus *task.Status
	if t.Status != task.StatusInProgress {
		nextStatus = task.Ptr(task.StatusInProgress)
	}
	if err := o.tasks.AddRunWithStatus(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      string(agent.RoleImplementation),
		Mode:      effMode,
		Provider:  ag.Provider,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}, nextStatus); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

// StartChat creates a synthetic chat task bound to projectID, prepares a
// dedicated (local-only) worktree, and launches an interactive agent with
// the requested provider. Rolls back on any failure so no orphans leak.
func (o *AgentOrchestrator) StartChat(projectID, providerName, prompt string) (*agent.Agent, error) {
	prov := strings.ToLower(strings.TrimSpace(providerName))
	if prov != "claude" && prov != "codex" {
		return nil, fmt.Errorf("invalid provider %q: must be claude or codex", providerName)
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if _, err := o.projects.Get(projectID); err != nil {
		return nil, fmt.Errorf("project %s: %w", projectID, err)
	}

	t, err := o.tasks.CreateChat(projectID)
	if err != nil {
		return nil, fmt.Errorf("create chat task: %w", err)
	}

	dir, err := o.worktrees.PrepareForChat(t)
	if err != nil {
		if delErr := o.tasks.Delete(t.ID); delErr != nil {
			o.logger.Error("chat.rollback.delete-task", "task_id", t.ID, "err", delErr)
		}
		return nil, fmt.Errorf("prepare chat worktree: %w", err)
	}

	requirePerm := resolvePermission(t, o.cfg)
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:             t.ID,
		Name:               t.Title,
		Mode:               "interactive",
		Provider:           prov,
		Prompt:             prompt,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
	})
	if err != nil {
		o.worktrees.Remove(t.ID)
		if delErr := o.tasks.Delete(t.ID); delErr != nil {
			o.logger.Error("chat.rollback.delete-task", "task_id", t.ID, "err", delErr)
		}
		return nil, err
	}

	o.logAudit(audit.EventAgentStarted, t.ID, ag.ID, map[string]any{
		"mode": "interactive", "title": t.Title, "role": "chat",
		"task_type": string(t.TaskType), "provider": ag.Provider,
		"require_permissions": requirePerm,
	})
	if err := o.tasks.AddRun(t.ID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      "chat",
		Mode:      "interactive",
		Provider:  ag.Provider,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		o.logger.Error("chat.add-run", "task_id", t.ID, "err", err)
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
