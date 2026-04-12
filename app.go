package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/logging"
	"github.com/Automaat/synapse/internal/loopagent"
	"github.com/Automaat/synapse/internal/notification"
	"github.com/Automaat/synapse/internal/poll"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/spotlight"
	"github.com/Automaat/synapse/internal/stats"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/watchdog"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const maxResultLen = 2000

type App struct {
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	tasks           *task.Manager
	projects        *project.Store
	loopAgents      *loopagent.Store
	loopSched       *loopagent.Scheduler
	agents          *agent.Manager
	tmux            *tmux.Manager
	watcher         *watcher.Watcher
	notifier        *notification.Emitter
	audit           *audit.Logger
	stats           *stats.Store
	tasksDir        string
	skillsDir       string
	repoDir         string
	worktreesDir    string
	logger          *slog.Logger
	logDir          string
	auditDir        string
	prTracker       *github.IssueTracker
	worktrees       *worktree.Manager
	agentOrch       *AgentOrchestrator
	reviewer        *ReviewHandler
	workflowEngine  *workflow.Engine
	workflowStore   *workflow.Store
	todoistHandler  *poll.TodoistHandler
	todoistCancel   context.CancelFunc
	renovateHandler *poll.RenovateHandler
	cfg             *config.Config
	logLevel        *slog.LevelVar
	emit            func(string, any)
	restartStaleErr *logging.ErrorThrottle

	// Wails-bound services (created in startup)
	taskSvc      *TaskService
	planSvc      *PlanningService
	agentSvc     *AgentService
	orchSvc      *OrchestratorService
	projectSvc   *ProjectService
	loopAgentSvc *LoopAgentService
	configSvc    *ConfigService
	intgSvc      *IntegrationService
	statsSvc     *StatsService
	reviewSvc    *ReviewService
	workflowSvc  *WorkflowService
}

func NewApp(logger *slog.Logger, logLevel *slog.LevelVar, cfg *config.Config) *App {
	a := &App{
		tasksDir:        cfg.TasksDir,
		skillsDir:       cfg.SkillsDir,
		repoDir:         cfg.RepoDir,
		worktreesDir:    cfg.WorktreesDir,
		logger:          logger,
		logDir:          cfg.Logging.Dir,
		auditDir:        cfg.AuditDir(),
		cfg:             cfg,
		logLevel:        logLevel,
		restartStaleErr: logging.NewErrorThrottle(),
	}
	// Pre-allocate service structs so Wails can bind them before startup().
	// Fields are populated in startup() once dependencies are initialized.
	a.taskSvc = &TaskService{}
	a.planSvc = &PlanningService{}
	a.agentSvc = &AgentService{}
	a.orchSvc = &OrchestratorService{}
	a.projectSvc = &ProjectService{}
	a.loopAgentSvc = &LoopAgentService{}
	a.configSvc = &ConfigService{}
	a.intgSvc = &IntegrationService{}
	a.statsSvc = &StatsService{}
	a.reviewSvc = &ReviewService{}
	a.workflowSvc = &WorkflowService{}
	return a
}

func (a *App) startup(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.logger.Info("app.starting")

	a.initAudit()

	a.initStats()

	store, err := task.NewStore(a.tasksDir)
	if err != nil {
		a.logger.Error("task.store.init", "err", err)
		runtime.Quit(ctx)
		return
	}

	projStore, err := project.NewStore(
		filepath.Join(config.HomeDir(), "projects"),
		filepath.Join(config.HomeDir(), "clones"),
	)
	if err != nil {
		a.logger.Error("project.store.init", "err", err)
		runtime.Quit(ctx)
		return
	}
	a.projects = projStore

	if err := a.initLoopAgents(); err != nil {
		runtime.Quit(ctx)
		return
	}
	a.tmux = tmux.NewManager()
	emit := func(event string, data any) {
		runtime.EventsEmit(ctx, event, data)
	}
	a.emit = emit
	a.emitDegradedWarnings(emit)
	a.tasks = task.NewManager(store, task.EmitterFunc(emit))
	a.initStatusHook()
	a.notifier = notification.New(emit)
	a.notifier.SetDesktop(a.cfg.Notification.Desktop)
	a.agents = agent.NewManager(ctx, a.tmux, emit, a.logger, a.logDir)
	a.agents.SetDefaultProvider(a.cfg.Agent.Provider)

	a.prTracker = github.NewIssueTracker(30 * time.Minute)

	// Initialize domain services (dependency order: worktrees → agentOrch → reviewer, workflow)
	a.worktrees = worktree.New(worktree.Config{
		WorktreesDir:     a.worktreesDir,
		Projects:         a.projects,
		Tasks:            a.tasks,
		Logger:           a.logger,
		PRBranchResolver: github.FetchPRBranch,
		AgentChecker:     a.agents.HasRunningAgentForTask,
	})
	a.agentOrch = newAgentOrchestrator(a.tasks, a.projects, a.agents, a.audit, a.logger, a.worktrees, a.cfg)
	a.reviewer = newReviewHandler(a.tasks, a.projects, a.agents, a.audit, a.logger, a.prTracker, emit, a.worktrees)

	a.initWorkflowEngine()

	a.agents.SetMaxConcurrent(a.cfg.Agent.MaxConcurrent)
	a.agents.SetGuardrails(agent.Guardrails{
		MaxCostUSD: a.cfg.Agent.MaxCostUSD,
		MaxTurns:   a.cfg.Agent.MaxTurns,
	})
	a.initApprovalServer(emit)
	a.agents.SetOnComplete(a.onAgentComplete)

	a.initLoopScheduler(ctx, emit)
	w := watcher.New(a.tasksDir, emit, a.logger)
	a.watcher = w
	if err := w.Start(ctx); err != nil {
		a.logger.Error("watcher.start", "err", err)
	}

	a.initTodoist(emit)
	a.initRenovate(emit)
	issuesFetcher := poll.NewIssuesFetcher(a.tasks, a.projects, emit, a.logger)
	a.wireServices(emit)

	a.syncSkills()
	a.reconnectAgents()
	a.worktrees.CleanupOrphaned()
	a.cleanStaleRuns()
	a.restartStaleInProgress()
	a.RegisterSpotlightHotkey()
	a.wg.Go(func() { a.orchestratorLoop(ctx) })

	wdog := watchdog.New(a.agents, a.tasks, a.logger, emit, &a.wg)
	a.wg.Go(func() { wdog.Run(ctx) })

	hub := poll.NewHub()
	hub.Register(a.reviewer, 10*time.Second)
	hub.Register(issuesFetcher, 20*time.Second)
	if a.renovateHandler != nil {
		hub.Register(a.renovateHandler, 15*time.Second)
	}
	hub.Start(ctx, &a.wg, a.logger)
	a.startTodoistLoop(ctx)
	a.logger.Info("app.started")
}

func (a *App) initStats() {
	statsStore, err := stats.NewStore(config.StatsFile())
	if err != nil {
		a.logger.Warn("stats.init.degraded", "err", err)
		// a.stats remains nil; StatsService.GetStats() guards against nil.
		return
	}
	a.stats = statsStore
	if err := statsStore.Backfill(a.auditDir); err != nil {
		a.logger.Warn("stats.backfill", "err", err)
	}
}

func (a *App) initStatusHook() {
	a.tasks.SetStatusChangeHook(func(taskID, from, to string) {
		a.logAudit(audit.EventTaskStatusChanged, taskID, "", map[string]any{"from": from, "to": to})

		// Advance workflows whose current run_agent step declares a
		// matching wait_for_status. This is how interactive agents (which
		// never exit between turns) signal step completion.
		if a.workflowEngine != nil {
			a.workflowEngine.HandleStatusChange(taskID, to)
		}

		switch to {
		case string(task.StatusInReview):
			msg := taskID
			if t, err := a.tasks.Get(taskID); err == nil {
				msg = t.Title
			}
			a.notifier.Send(notification.LevelInfo, "Ready for review", msg, taskID, "")
		case string(task.StatusHumanRequired):
			msg := taskID
			if t, err := a.tasks.Get(taskID); err == nil {
				msg = t.Title
			}
			a.notifier.Send(notification.LevelWarning, "Needs human", msg, taskID, "")
		case string(task.StatusTesting):
			if a.workflowEngine != nil {
				if _, err := a.workflowEngine.DispatchEvent(
					taskID,
					"task.status_changed",
					map[string]string{"task.status": string(task.StatusTesting)},
					nil,
				); err != nil {
					a.logger.Error("workflow.dispatch.testing", "task_id", taskID, "err", err)
				}
			}
		}
	})
}

// wireServices populates the Wails-bound service structs that were pre-allocated
// in NewApp(). Must be called after all dependencies are initialized.
func (a *App) initAudit() {
	al, err := audit.NewLogger(a.auditDir)
	if err != nil {
		a.logger.Warn("audit.init.degraded", "err", err)
		// a.audit remains nil; logAudit() is a no-op when audit is nil.
		return
	}
	a.audit = al
	retentionDays := a.cfg.Audit.RetentionDays
	if retentionDays <= 0 {
		retentionDays = 30
	}
	if err := audit.Cleanup(a.auditDir, retentionDays); err != nil {
		a.logger.Warn("audit.cleanup", "err", err)
	}
}

// emitDegradedWarnings fires startup:degraded for any subsystem that failed
// to initialize. Called after emit is configured so the frontend receives the events.
func (a *App) emitDegradedWarnings(emit func(string, any)) {
	type degraded struct {
		Subsystem string `json:"subsystem"`
		Reason    string `json:"reason"`
	}
	if a.audit == nil {
		emit(events.StartupDegraded, degraded{"audit", "audit logger failed to initialize; audit trail unavailable"})
	}
	if a.stats == nil {
		emit(events.StartupDegraded, degraded{"stats", "stats store failed to initialize; metrics unavailable"})
	}
}

func (a *App) onAgentComplete(ag *agent.Agent) {
	var resultContent string
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			resultContent = ev.Content
		}
	}

	// Snapshot mutable fields once under the agent's lock so both the
	// persistence write and the audit entry see a consistent view.
	state := ag.GetState()
	cost := ag.GetCostUSD()
	exitErr := ag.GetExitErr()

	// Audit logging always fires — orchestrator brain agents have no parent
	// task and skip the storage paths below, but their lifecycle still
	// belongs in the audit trail.
	duration := time.Since(ag.StartedAt).Seconds()
	a.logAudit(audit.EventAgentCompleted, ag.TaskID, ag.ID, map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   cost,
		"duration_s": duration,
		"state":      string(state),
		"role":       agent.RoleFromName(ag.Name),
		"provider":   ag.Provider,
		"name":       ag.Name,
	})

	// Loop agents run without a TaskID — let the scheduler record cost
	// before the early return below kicks in.
	if a.loopSched != nil {
		a.loopSched.OnAgentComplete(ag)
	}

	// Orchestrator brain agents run with TaskID="" (rooted at ~/.synapse,
	// no parent task). Calling UpdateRun / HandleAgentComplete / Get with
	// an empty ID joins to ".synapse/tasks/.md" and crashes the handler.
	if ag.TaskID == "" {
		return
	}

	// Persist run result to task file.
	truncated := resultContent
	if len(truncated) > maxResultLen {
		truncated = truncated[:maxResultLen] + "\n... (truncated)"
	}
	if err := a.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":    string(state),
		"cost_usd": cost,
		"result":   truncated,
		"log_file": ag.LogPath,
	}); err != nil {
		a.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

	// Advance workflow.
	if a.workflowEngine != nil {
		a.workflowEngine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID: ag.ID,
			Result:  resultContent,
			Success: exitErr == nil,
		})
	}

	// Worktree cleanup for done tasks (after engine advances, so status is final).
	if t, err := a.tasks.Get(ag.TaskID); err == nil && t.Status == task.StatusDone {
		go a.worktrees.Remove(ag.TaskID)
	}
}

func (a *App) initWorkflowEngine() {
	wfStore, err := workflow.NewStore(config.WorkflowsDir())
	if err != nil {
		a.logger.Error("workflow.store.init", "err", err)
		return
	}
	a.workflowStore = wfStore
	if syncErr := workflow.SyncBuiltins(wfStore); syncErr != nil {
		a.logger.Error("workflow.sync-builtins", "err", syncErr)
	}
	a.workflowEngine = workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: a.tasks},
		&agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks},
		a.logger,
	)
	a.workflowEngine.SetPRLinker(prLinkerAdapter{})
	a.workflowEngine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetContext(a.ctx)
}

func (a *App) initApprovalServer(emit func(string, any)) {
	srv, err := agent.NewApprovalServer(emit, a.logger)
	if err != nil {
		a.logger.Error("approval-server.init", "err", err)
		return
	}
	srv.SetManager(a.agents)
	a.agents.SetApprovalAddr(srv.Addr())
	a.agentSvc.approval = srv
}

func (a *App) wireServices(emit func(string, any)) {
	a.reviewer.workflowEngine = a.workflowEngine
	a.reviewSvc.reviewer = a.reviewer
	a.reviewSvc.tasks = a.tasks
	a.taskSvc.tasks = a.tasks
	a.taskSvc.agents = a.agents
	a.taskSvc.workflowEngine = a.workflowEngine
	a.taskSvc.worktrees = a.worktrees
	a.taskSvc.wg = &a.wg
	a.taskSvc.logger = a.logger
	a.taskSvc.audit = a.audit
	a.planSvc.engine = a.workflowEngine
	a.planSvc.tasks = a.tasks
	a.planSvc.agents = a.agents
	a.agentSvc.agents = a.agents
	a.agentSvc.tmux = a.tmux
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit
	a.orchSvc.cfg = a.cfg
	a.projectSvc.projects = a.projects
	a.projectSvc.worktrees = a.worktrees
	a.projectSvc.logger = a.logger
	a.loopAgentSvc.store = a.loopAgents
	a.loopAgentSvc.sched = a.loopSched
	a.loopAgentSvc.auditDir = a.auditDir
	a.loopAgentSvc.logger = a.logger
	a.configSvc.cfg = a.cfg
	a.configSvc.logLevel = a.logLevel
	a.configSvc.notifier = a.notifier
	a.configSvc.agents = a.agents
	a.configSvc.reloadHook = a.reloadTodoist
	a.intgSvc.tasks = a.tasks
	a.intgSvc.projects = a.projects
	a.intgSvc.agents = a.agents
	a.intgSvc.worktrees = a.worktrees
	a.intgSvc.audit = a.audit
	a.intgSvc.cfg = a.cfg
	a.intgSvc.logger = a.logger
	a.intgSvc.todoistHandler = a.todoistHandler
	a.intgSvc.renovateHandler = a.renovateHandler
	a.intgSvc.workflowEngine = a.workflowEngine
	a.statsSvc.stats = a.stats
	a.workflowSvc.engine = a.workflowEngine
	a.workflowSvc.store = a.workflowStore
}

func (a *App) shutdown(_ context.Context) {
	a.logger.Info("app.stopping")
	if a.loopSched != nil {
		a.loopSched.Stop()
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.agents.Shutdown()
	if a.audit != nil {
		_ = a.audit.Close()
	}
	a.logger.Info("app.stopped")
}

func (a *App) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		a.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

func (a *App) reconnectAgents() {
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Warn("reconnect.tasks", "err", err)
		return
	}

	var infos []agent.TaskInfo
	for i := range tasks {
		if tasks[i].Status == task.StatusInProgress {
			infos = append(infos, agent.TaskInfo{ID: tasks[i].ID, Title: tasks[i].Title})
		}
	}

	n := a.agents.ReconnectSessions(infos)
	if n > 0 {
		a.logger.Info("reconnect.done", "count", n)
	}
}

// cleanStaleRuns marks agent_runs still showing "running" as "stopped" if no
// matching in-memory agent exists. Fixes leftover state from crashes/restarts.
func (a *App) cleanStaleRuns() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		for j := range tasks[i].AgentRuns {
			run := &tasks[i].AgentRuns[j]
			if run.State != string(agent.StateRunning) {
				continue
			}
			if a.agents.HasRunningAgentForTask(tasks[i].ID) {
				continue
			}
			a.logger.Info("stale-run.cleanup", "task_id", tasks[i].ID, "agent_id", run.AgentID)
			_ = a.tasks.UpdateRun(tasks[i].ID, run.AgentID, map[string]any{
				"state":  string(agent.StateStopped),
				"result": "stale: marked stopped on startup",
			})
		}
	}
}

// restartStaleMinAge is the minimum age of the latest agent run before a
// stale in-progress task is eligible for respawn. Protects against dev-mode
// hot-reload loops spawning parallel agents onto the same task.
const restartStaleMinAge = 5 * time.Minute

// restartStaleInProgress recovers in-progress tasks that lost their agent
// due to a crash, restart, or tmux session death. Headless tasks are
// re-dispatched; interactive tasks drive the workflow engine forward via
// recoverStaleInteractive (no new tmux session is spawned).
func (a *App) restartStaleInProgress() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if slices.Contains(t.Tags, "review") {
			continue
		}
		// Skip tasks whose workflow already finished. The workflow engine
		// is the source of truth for when work is done; re-spawning an
		// agent here would loop forever because the agent's completion
		// callback can't advance a terminal workflow. Operators can drive
		// the task out of this state manually (e.g. flip to in-review so
		// pr-monitor picks it up, or reset to todo to restart).
		if t.Workflow != nil &&
			(t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed) {
			a.logger.Debug("restart-stale.skip",
				"task_id", t.ID, "reason", "workflow_terminal",
				"state", string(t.Workflow.State))
			continue
		}
		// Debounce respawn when a previous run started recently. Covers the
		// dev-reload case: app restarts every few seconds, but a headless
		// subprocess from the prior lifecycle is still alive.
		if lr := lastAgentRun(&t); lr != nil && time.Since(lr.StartedAt) < restartStaleMinAge {
			a.logger.Info("restart-stale.skip",
				"task_id", t.ID, "reason", "recent_run",
				"last_run_age_s", time.Since(lr.StartedAt).Seconds())
			continue
		}
		// Tasks whose last agent was a pr-fix should not be re-implemented.
		// Move them back to in-review so the reviews poller can re-detect and fix.
		// Applies to both headless and interactive modes — handlePRIssue
		// spawns pr-fix agents directly without registering a workflow, so
		// onAgentComplete can't advance the task back to in-review itself.
		if lastRun := lastAgentRun(&t); lastRun != nil && lastRun.Role == "pr-fix" {
			a.logger.Info("restart-stale.revert-to-review", "task_id", t.ID)
			if _, err := a.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
				a.logger.Error("restart-stale.revert", "task_id", t.ID, "err", err)
			}
			continue
		}
		// Interactive: don't spawn a new tmux session automatically. Instead
		// drive the workflow engine to advance the current step using the
		// stored agent run result — same mechanism as onAgentComplete.
		if t.AgentMode != "headless" {
			a.recoverStaleInteractive(&t)
			continue
		}
		if t.ProjectID == "" {
			a.logger.Warn("restart-stale.skip", "task_id", t.ID, "reason", "no project_id")
			continue
		}
		a.logger.Info("restart.stale-in-progress", "task_id", t.ID, "run_role", t.RunRole)
		taskID := t.ID
		runRole := t.RunRole
		if runRole == "pr-fix" {
			a.wg.Go(func() {
				err := a.agentOrch.StartPRFixAgent(taskID)
				a.restartStaleErr.Log(a.logger, "restart.pr-fix.failed", "pr-fix:"+taskID, err, "task_id", taskID)
			})
		} else {
			mode := t.AgentMode
			a.wg.Go(func() {
				// Restart-stale only ever reaches this branch for headless
				// mode (interactive tasks are handled by recoverStaleInteractive
				// above), so OneShot is irrelevant here — pass false.
				_, err := a.agentOrch.StartAgent(taskID, mode, "Continue implementing this task. When done, create a draft PR with `gh pr create --draft`.", false)
				a.restartStaleErr.Log(a.logger, "restart-stale.failed", "stale:"+taskID, err, "task_id", taskID)
			})
		}
	}
}

// recoverStaleInteractive handles interactive in-progress tasks whose tmux
// session died or disappeared across restarts. Marks the last agent run as
// stopped (if still claiming running) and drives the workflow engine to
// advance the current step using the stored result — mirroring the normal
// onAgentComplete callback so evaluate/next steps fire.
func (a *App) recoverStaleInteractive(t *task.Task) {
	lr := lastAgentRun(t)
	if lr == nil {
		a.logger.Info("recover-stale.skip", "task_id", t.ID, "reason", "no_agent_runs")
		return
	}
	// Only recover when the dead agent was interactive — headless stragglers
	// (triage/eval) are managed by their own error paths, and we don't want
	// to fake-complete a workflow step that needs real agent output.
	if lr.Mode != "interactive" {
		return
	}
	if lr.State == string(agent.StateRunning) {
		if err := a.tasks.UpdateRun(t.ID, lr.AgentID, map[string]any{
			"state":  string(agent.StateStopped),
			"result": "stale: agent gone, auto-recovered",
		}); err != nil {
			a.logger.Error("recover-stale.update-run", "task_id", t.ID, "err", err)
		}
	}
	if a.workflowEngine == nil || t.Workflow == nil {
		a.logger.Info("recover-stale.no-workflow", "task_id", t.ID)
		return
	}
	if t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed {
		a.logger.Info("recover-stale.workflow-terminal",
			"task_id", t.ID, "state", string(t.Workflow.State))
		return
	}
	// Mark the execution as recovered so the next step's template context
	// knows not to trust .Prev.Output (use recoveredOrPrev instead). Persist
	// before driving HandleAgentComplete so the engine reloads the flag.
	t.Workflow.Recovered = true
	wf := t.Workflow
	if _, err := a.tasks.Update(t.ID, task.Update{Workflow: &wf}); err != nil {
		a.logger.Error("recover-stale.set-recovered", "task_id", t.ID, "err", err)
		return
	}
	a.logger.Info("recover-stale.advance",
		"task_id", t.ID, "agent_id", lr.AgentID, "step", t.Workflow.CurrentStep)
	a.workflowEngine.HandleAgentComplete(t.ID, workflow.AgentCompletion{
		AgentID: lr.AgentID,
		Success: true,
	})
}

func lastAgentRun(t *task.Task) *task.AgentRun {
	if len(t.AgentRuns) == 0 {
		return nil
	}
	return &t.AgentRuns[len(t.AgentRuns)-1]
}

// StartAgent delegates to AgentOrchestrator and is exposed as a Wails-bound method.
// User-triggered starts are never one-shot — that flag is reserved for workflow
// steps that expect a single turn.
func (a *App) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	return a.agentOrch.StartAgent(taskID, mode, prompt, false)
}

// seedDefaultLoopAgents creates the built-in synapse-self-monitor loop on
// first boot only. It is disabled by default so the user can review the
// configuration in the GUI before enabling. Idempotent: if a record with
// the same Name already exists this is a no-op.
func (a *App) initLoopAgents() error {
	store, err := loopagent.NewStore(a.cfg.LoopAgentsDir)
	if err != nil {
		a.logger.Error("loopagent.store.init", "err", err)
		return err
	}
	a.loopAgents = store
	return nil
}

func (a *App) initLoopScheduler(ctx context.Context, emit func(string, any)) {
	a.loopSched = loopagent.NewScheduler(ctx, a.loopAgents, a.agents, a.logger, emit, config.HomeDir())
	a.seedDefaultLoopAgents()
	a.loopSched.Sync()
}

func (a *App) seedDefaultLoopAgents() {
	if a.loopAgents == nil {
		return
	}
	const name = "synapse-self-monitor"
	if _, ok := a.loopAgents.FindByName(name); ok {
		return
	}
	created, err := a.loopAgents.Create(loopagent.LoopAgent{
		Name:         name,
		Prompt:       "/synapse-self-monitor",
		IntervalSec:  21600, // 6 hours
		AllowedTools: []string{"Bash", "Read", "Grep", "Glob"},
		Provider:     "claude",
		Model:        "sonnet",
		Enabled:      false,
	})
	if err != nil {
		a.logger.Warn("loopagent.seed.failed", "name", name, "err", err)
		return
	}
	a.logger.Info("loopagent.seed.created", "id", created.ID, "name", name)
}

func (a *App) syncSkills() {
	repoDir := a.repoDir
	if repoDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			a.logger.Error("skills.sync.skip", "reason", "no repo_dir and cannot get cwd")
			return
		}
		repoDir = cwd
		a.logger.Info("skills.sync.fallback_cwd", "dir", cwd)
	}

	a.logger.Info("skills.sync.start", "src", repoDir, "dst", a.skillsDir)

	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	a.syncDir(skillsSrc, a.skillsDir)

	claudeSrc := filepath.Join(repoDir, "orchestrator", "CLAUDE.md")
	claudeDst := filepath.Join(config.HomeDir(), "CLAUDE.md")
	a.syncFile(claudeSrc, claudeDst)
	a.syncFile(claudeSrc, filepath.Join(config.HomeDir(), "AGENTS.md"))

	a.logger.Info("skills.sync.done")
}

func (a *App) syncDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		a.logger.Debug("sync.skip", "src", src, "reason", err)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		a.logger.Error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	cleanSrc := filepath.Clean(src) + string(filepath.Separator)
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		// Reject symlinks — they could point outside the destination directory.
		if e.Type()&fs.ModeSymlink != 0 {
			a.logger.Debug("sync.skip.symlink", "name", e.Name())
			continue
		}
		srcPath := filepath.Join(filepath.Clean(src), e.Name())
		dstPath := filepath.Join(filepath.Clean(dst), e.Name())
		// Canonicalize and guard against crafted entry names escaping the roots.
		if !strings.HasPrefix(srcPath+string(filepath.Separator), cleanSrc) ||
			!strings.HasPrefix(dstPath+string(filepath.Separator), cleanDst) {
			a.logger.Warn("sync.skip.traversal", "name", e.Name())
			continue
		}
		a.syncFile(srcPath, dstPath)
		srcNames[e.Name()] = struct{}{}
	}

	// Remove orphan .md files in dst that no longer exist in src.
	dstEntries, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range dstEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		if _, ok := srcNames[e.Name()]; ok {
			continue
		}
		dstPath := filepath.Join(filepath.Clean(dst), e.Name())
		if !strings.HasPrefix(dstPath+string(filepath.Separator), cleanDst) {
			continue
		}
		if err := os.Remove(dstPath); err != nil {
			a.logger.Warn("sync.orphan.remove.fail", "file", e.Name(), "err", err)
		} else {
			a.logger.Info("sync.orphan.removed", "file", e.Name())
		}
	}
}

func (a *App) syncFile(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		a.logger.Debug("sync.read.skip", "src", src, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		a.logger.Error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	if err := os.WriteFile(dst, data, fs.FileMode(0o644)); err != nil {
		a.logger.Error("sync.write", "dst", dst, "err", err)
		return
	}
	a.logger.Info("sync.copied", "file", filepath.Base(dst))
}

// ListNotifications returns pending in-app notifications.
func (a *App) ListNotifications() []notification.Notification {
	return a.notifier.List()
}

// SetDesktopNotifications enables or disables macOS desktop notifications.
func (a *App) SetDesktopNotifications(enabled bool) {
	a.notifier.SetDesktop(enabled)
}

// RegisterSpotlightHotkey binds Ctrl+Space to the Spotlight quick-add panel.
func (a *App) RegisterSpotlightHotkey() {
	spotlight.OnSubmit(func(title, projectID string) {
		a.logger.Info("spotlight.submit", "title", title, "project", projectID)
		go func() {
			t, err := a.taskSvc.CreateTask(title, "", "headless")
			if err != nil {
				a.logger.Error("spotlight.create", "err", err)
				return
			}
			if projectID != "" {
				if _, err := a.taskSvc.UpdateTask(t.ID, map[string]any{"project_id": projectID}); err != nil {
					a.logger.Error("spotlight.project", "err", err)
				}
			}
		}()
	})

	if err := spotlight.Register(func() {
		projectsJSON := "[]"
		if projects, err := a.projectSvc.ListProjects(); err == nil {
			if data, err := json.Marshal(projects); err == nil {
				projectsJSON = string(data)
			}
		}
		spotlight.ShowPanel(projectsJSON)
	}); err != nil {
		a.logger.Error("spotlight.register", "err", err)
		return
	}
	a.logger.Info("spotlight.registered", "hotkey", "ctrl+space")
}
