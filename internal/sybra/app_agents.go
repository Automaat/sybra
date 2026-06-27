package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
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
	for i := range slices.Backward(runs) {
		run := &runs[i]
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
	bgops     *bgop.Tracker
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

// sandboxEnvIfRunning returns the sandbox env vars only when a sandbox is
// already running for the task; it never starts one. Sandboxes are started
// lazily by the testing phase (test-runner role), so implementation/review
// agents inherit one only if testing left it up — they never spin a cluster.
func (o *AgentOrchestrator) sandboxEnvIfRunning(taskID string) []string {
	if o.sandboxes == nil {
		return nil
	}
	if inst := o.sandboxes.Get(taskID); inst != nil {
		return inst.EnvVars()
	}
	return nil
}

// sandboxEnv resolves the extra environment variables a task's configured
// sandbox injects into its agent subprocess, starting the sandbox on demand.
// Returns nil when no sandbox applies or startup fails (a failed start is
// logged, not fatal — the agent runs without the sandbox env). Called from the
// testing phase so the per-task sandbox spins up only when tests actually run.
func (o *AgentOrchestrator) sandboxEnv(taskID, dir string, t task.Task) []string {
	if o.sandboxes == nil || t.ProjectID == "" {
		return nil
	}
	proj, pErr := o.projects.Get(t.ProjectID)
	if pErr != nil || proj.Sandbox == nil {
		return nil
	}
	inst := o.sandboxes.Get(taskID)
	if inst == nil {
		newInst, startErr := o.sandboxes.Start(context.Background(), taskID, dir, proj.Sandbox)
		if startErr != nil {
			o.logger.Warn("sandbox.start.failed", "task_id", taskID, "err", startErr)
			return nil
		}
		inst = newInst
	}
	return inst.EnvVars()
}

// prependSupervisorSteer consumes a pending watchdog headless-nudge steer for
// taskID: it clears the one-shot SupervisorSteer field and returns prompt with
// the correction prepended. Returns prompt unchanged when none is pending.
// Called at the head of the headless re-dispatch entry points (this orchestrator,
// the workflow run_agent path, and the pr-fix agent) so the first agent resumed
// after a nudge carries the correction and later ones do not. The orchestrator
// resume loops (ResumeStalled then RestartStaleInProgress) run sequentially on a
// single goroutine and each gates on no-running-agent before dispatching, so the
// read-then-clear is not raced by a concurrent dispatcher for the same task.
//
// The prompt is steered ONLY after the clear succeeds, so a failed clear leaves
// the steer pending (the error is returned, the prompt is unchanged) rather than
// applying it twice — preserving the one-shot contract. A start that then fails
// loses the nudge, which is recoverable: the watchdog re-nudges if the resumed
// agent loops again.
func prependSupervisorSteer(tasks *task.Manager, taskID, prompt string) (string, error) {
	t, err := tasks.Get(taskID)
	if err != nil {
		// Cannot read the task → cannot consume a steer; dispatch unsteered and
		// let the caller log. Any pending steer stays for a later dispatch.
		return prompt, err
	}
	steer := strings.TrimSpace(t.SupervisorSteer)
	if steer == "" {
		return prompt, nil
	}
	if _, uErr := tasks.Update(taskID, task.Update{SupervisorSteer: task.Ptr("")}); uErr != nil {
		return prompt, fmt.Errorf("clear supervisor steer: %w", uErr)
	}
	return "Supervisor course-correction: " + steer + "\n\n" + prompt, nil
}

func (o *AgentOrchestrator) StartAgent(taskID, mode, prompt string, includeTaskDescription, oneShot bool) (*agent.Agent, error) {
	// Serialize dispatch per task. Held across the whole start — including the
	// multi-second worktree prep below, during which the agent is not yet
	// registered — so a concurrent dispatcher (recovery loop, ResumeStalled,
	// a manual start) cannot observe "no running agent" and launch a duplicate
	// on the same worktree. workflow.ErrDispatchInFlight is benign: the holder
	// will produce the task's agent.
	if !o.agents.ClaimTaskDispatch(taskID) {
		return nil, workflow.ErrDispatchInFlight
	}
	defer o.agents.ReleaseTaskDispatch(taskID)

	// Consume a pending watchdog headless-nudge steer (no-op when none). Held
	// within the dispatch claim so the read-then-clear is serialized per task.
	// On a clear failure the steer stays pending and we dispatch unsteered.
	if steered, sErr := prependSupervisorSteer(o.tasks, taskID, prompt); sErr != nil {
		o.logger.Warn("supervisor-steer.consume", "task_id", taskID, "err", sErr)
	} else {
		prompt = steered
	}

	t, err := o.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	// An umbrella tracker task runs no agent — it only rolls up its children.
	// Refuse here, the single dispatch choke point, so no path (workflow,
	// recovery, manual start) can launch an agent against it.
	if t.TaskType == task.TaskTypeUmbrella {
		return nil, fmt.Errorf("task %s is an umbrella tracker; it runs no agent", taskID)
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
		opID, onPhase := o.startWorktreeOp("Preparing worktree: "+t.Title, t.ProjectID, taskID)
		d, wtErr := o.worktrees.PrepareForTask(t, onPhase)
		if wtErr != nil {
			o.failWorktreeOp(opID, wtErr)
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		o.completeWorktreeOp(opID)
		dir = d
	}
	if dir == "" {
		return nil, fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Sybra cwd", taskID, skipWT)
	}

	var workflowStart time.Time
	if t.Workflow != nil {
		workflowStart = t.Workflow.StartedAt
	}
	resumeSessionID := pickImplementationResumeSession(t.AgentRuns, workflowStart)

	extraEnv := o.sandboxEnvIfRunning(taskID)

	fullPrompt := buildTaskStartPrompt(t, prompt, includeTaskDescription)
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
		MaxTurns:           t.MaxTurns,
		ForkSubagent:       t.ForkSubagent,
		ReasoningEffort:    t.ReasoningEffort,
		// Always an implementation run — prime it with the NOTES.md scratchpad.
		SeedWorkingMemory: true,
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
		Prompt:    fullPrompt,
	}, nextStatus); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

func buildTaskStartPrompt(t task.Task, prompt string, includeTaskDescription bool) string {
	prompt = strings.TrimSpace(prompt)
	if !includeTaskDescription {
		return prompt
	}
	base := fmt.Sprintf("# Task: %s\n\n%s", t.Title, t.Body)
	if prompt == "" {
		return base
	}
	return base + "\n\n---\n\n" + prompt
}

// StartChat creates a synthetic chat task bound to projectID, prepares a
// dedicated (local-only) worktree, and launches an interactive agent with
// the requested provider. Rolls back on any failure so no orphans leak.
func (o *AgentOrchestrator) StartChat(projectID, providerName, prompt string) (*agent.Agent, error) {
	prov := strings.ToLower(strings.TrimSpace(providerName))
	if prov != "claude" && prov != "codex" && prov != "copilot" {
		return nil, fmt.Errorf("invalid provider %q: must be claude, codex, or copilot", providerName)
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errors.New("project_id is required")
	}
	if _, err := o.projects.Get(projectID); err != nil {
		return nil, fmt.Errorf("project %s: %w", projectID, err)
	}

	t, err := o.tasks.CreateChat(projectID)
	if err != nil {
		return nil, fmt.Errorf("create chat task: %w", err)
	}

	opID, onPhase := o.startWorktreeOp("Preparing chat worktree", projectID, t.ID)
	dir, err := o.worktrees.PrepareForChat(t, onPhase)
	if err != nil {
		o.failWorktreeOp(opID, err)
		if delErr := o.tasks.Delete(t.ID); delErr != nil {
			o.logger.Error("chat.rollback.delete-task", "task_id", t.ID, "err", delErr)
		}
		return nil, fmt.Errorf("prepare chat worktree: %w", err)
	}
	o.completeWorktreeOp(opID)

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
		Prompt:    prompt,
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
	// Same per-task dispatch serialization as StartAgent — a pr-fix dispatch
	// must not race a concurrent implementation/recovery dispatch.
	if !o.agents.ClaimTaskDispatch(taskID) {
		return workflow.ErrDispatchInFlight
	}
	defer o.agents.ReleaseTaskDispatch(taskID)

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
		opID, onPhase := o.startWorktreeOp("Preparing worktree: "+t.Title, t.ProjectID, taskID)
		d, wtErr := o.worktrees.PrepareForTask(t, onPhase)
		if wtErr != nil {
			o.failWorktreeOp(opID, wtErr)
			return fmt.Errorf("worktree required: %w", wtErr)
		}
		o.completeWorktreeOp(opID)
		dir = d
	}
	if dir == "" {
		return fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Sybra cwd", taskID, skipWT)
	}

	prompt := buildPRFixPrompt(t, o.logger)
	if steered, sErr := prependSupervisorSteer(o.tasks, taskID, prompt); sErr != nil {
		o.logger.Warn("supervisor-steer.consume", "task_id", taskID, "err", sErr)
	} else {
		prompt = steered
	}
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               agent.RolePRFix.AgentName(t.Title),
		Mode:               effMode,
		Prompt:             prompt,
		AllowedTools:       t.AllowedTools,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
		// pr-fix is a code-author role — keep the NOTES.md contract airtight so
		// an adopted (handoff) worktree's scratchpad carries through.
		SeedWorkingMemory: agent.RolePRFix.AuthorsCode(),
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
		Prompt: prompt,
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
	base := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\nFix the issues raised in the PR review. Push the changes when done.\n\nNever weaken, skip, delete, comment out, or hardcode tests, snapshots, or fixtures to make checks pass, and never edit CI config to neuter a gate. Fix the underlying code; tampering is detected and blocks the task.", t.Title, t.Body)
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

// startWorktreeOp starts a bgop for worktree preparation and returns the op ID
// and a phase-update callback. Returns empty string and nil when bgops is nil.
func (o *AgentOrchestrator) startWorktreeOp(label, projectID, taskID string) (opID string, onPhase func(string)) {
	if o.bgops == nil {
		return "", nil
	}
	opID = o.bgops.Start(bgop.TypeWorktreePrep, label, projectID, taskID)
	onPhase = func(phase string) { o.bgops.UpdatePhase(opID, phase) }
	return opID, onPhase
}

func (o *AgentOrchestrator) completeWorktreeOp(opID string) {
	if o.bgops != nil && opID != "" {
		o.bgops.Complete(opID)
	}
}

func (o *AgentOrchestrator) failWorktreeOp(opID string, err error) {
	if o.bgops != nil && opID != "" {
		o.bgops.Fail(opID, err)
	}
}
