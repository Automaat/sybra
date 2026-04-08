package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/executil"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/worktree"
)

// resolveExecution derives the effective mode, directory, permission mode, and
// whether project worktree setup should be skipped based on the task's type.
// hintMode is used when the task type does not force a specific mode.
func resolveExecution(t task.Task, hintMode, researchMachineDir string) (mode, dir string, requirePerm, skipWorktree bool) {
	switch t.TaskType {
	case task.TaskTypeDebug:
		return "interactive", "", true, false
	case task.TaskTypeResearch:
		return "headless", researchMachineDir, false, true
	default:
		return hintMode, "", false, false
	}
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

func (o *AgentOrchestrator) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	t, err := o.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	researchDir := ""
	if o.cfg != nil {
		researchDir = o.cfg.Agent.ResearchMachineDir
	}
	effMode, dir, requirePerm, skipWT := resolveExecution(t, mode, researchDir)
	if !skipWT {
		t = o.autoAssignProject(t)
		if t.ProjectID != "" {
			d, wtErr := o.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
			}
			dir = d
		}
	}

	// Flip status only after worktree prep succeeds — otherwise a failed
	// prep leaves the task stuck at in-progress with no running agent,
	// which triggers restart-stale respawn loops on every app restart.
	if t.Status != task.StatusInProgress {
		if _, err := o.tasks.Update(taskID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
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
	})
	if err != nil {
		return nil, err
	}
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": effMode, "title": t.Title, "task_type": string(t.TaskType)})
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
	if _, err := o.tasks.Update(t.ID, map[string]any{"project_id": t.ProjectID}); err != nil {
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
	effMode, dir, requirePerm, skipWT := resolveExecution(t, t.AgentMode, researchDir)
	if !skipWT {
		t = o.autoAssignProject(t)
		if t.ProjectID != "" {
			d, wtErr := o.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return fmt.Errorf("worktree required: %w", wtErr)
			}
			dir = d
		}
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

	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": effMode, "title": t.Title, "role": "pr-fix", "task_type": string(t.TaskType)})
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

func openTmuxInGhostty(session, tabTitle string) error {
	label := "Synapse: " + tabTitle
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Synapse:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'printf \"\\033]0;%s\\007\"; exec tmux attach -t %s'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, executil.EscapeAppleScript(label), executil.EscapeAppleScript(session))
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}
