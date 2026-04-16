package sybra

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/selfmonitor"
	"github.com/Automaat/sybra/internal/spotlight"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/watchdog"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
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
	providerHealth  *provider.Checker
	worktrees       *worktree.Manager
	sandboxes       *sandbox.Manager
	monitorSvc      *monitor.Service
	selfMonitorSvc  *selfmonitor.Service
	agentOrch       *AgentOrchestrator
	reviewer        *ReviewHandler
	workflowEngine  *workflow.Engine
	workflowStore   *workflow.Store
	todoistHandler  *poll.TodoistHandler
	todoistCancel   context.CancelFunc
	renovateHandler *poll.RenovateHandler
	triageHandler   *poll.TriageHandler
	cfg             *config.Config
	logLevel        *slog.LevelVar
	emit            func(string, any)
	emitFactory     func(context.Context) func(string, any)
	restartStaleErr *logging.ErrorThrottle

	bgops *bgop.Tracker

	// skillsFS is an optional embedded FS used as a fallback when the
	// repository's .claude/skills/ directory is not present on disk.
	skillsFS fs.FS

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
	infoSvc      *InfoService
}

// Option configures App behaviour at construction time.
type Option func(*App)

// WithEmitFactory sets a factory that produces the emit function once the
// startup context is available. Used by Wails (needs ctx for EventsEmit) and
// the HTTP server (broker.Emit, ignores ctx).
func WithEmitFactory(fn func(context.Context) func(string, any)) Option {
	return func(a *App) { a.emitFactory = fn }
}

// WithEmit is a convenience option for emit functions that don't need ctx.
func WithEmit(fn func(string, any)) Option {
	return func(a *App) {
		a.emitFactory = func(context.Context) func(string, any) { return fn }
	}
}

// WithSkillsFS sets an embedded FS used as a fallback source for skill files
// when the repository's .claude/skills/ directory is not present on disk.
// Typically populated from the internal/skills package in the server binary.
func WithSkillsFS(skillsFS fs.FS) Option {
	return func(a *App) { a.skillsFS = skillsFS }
}

// ServiceRegistry returns the named service instances for HTTP dispatch.
// All values are the concrete pointers Wails binds; the HTTP handler uses
// reflection to call their exported methods.
func (a *App) ServiceRegistry() map[string]any {
	return map[string]any{
		"App":                 a,
		"AgentService":        a.agentSvc,
		"ConfigService":       a.configSvc,
		"InfoService":         a.infoSvc,
		"IntegrationService":  a.intgSvc,
		"LoopAgentService":    a.loopAgentSvc,
		"OrchestratorService": a.orchSvc,
		"PlanningService":     a.planSvc,
		"ProjectService":      a.projectSvc,
		"ReviewService":       a.reviewSvc,
		"StatsService":        a.statsSvc,
		"TaskService":         a.taskSvc,
		"WorkflowService":     a.workflowSvc,
	}
}

func NewApp(logger *slog.Logger, logLevel *slog.LevelVar, cfg *config.Config, opts ...Option) *App {
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
	a.infoSvc = &InfoService{}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Startup initializes all subsystems. Returns an error if a critical subsystem
// fails; callers (Wails OnStartup, HTTP server main) handle the error.
func (a *App) Startup(ctx context.Context) error {
	ctx, a.cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.logger.Info("app.starting")

	a.initAudit()
	a.initStats()

	store, err := task.NewStore(a.tasksDir)
	if err != nil {
		a.logger.Error("task.store.init", "err", err)
		return fmt.Errorf("task store: %w", err)
	}

	projStore, err := project.NewStore(
		filepath.Join(config.HomeDir(), "projects"),
		filepath.Join(config.HomeDir(), "clones"),
	)
	if err != nil {
		a.logger.Error("project.store.init", "err", err)
		return fmt.Errorf("project store: %w", err)
	}
	a.projects = projStore

	if err := a.initLoopAgents(); err != nil {
		return fmt.Errorf("loop agents: %w", err)
	}
	if a.emitFactory != nil {
		a.emit = a.emitFactory(ctx)
	} else {
		a.emit = func(string, any) {}
	}
	emit := func(event string, data any) {
		switch event {
		case events.TaskCreated, events.TaskUpdated, events.TaskDeleted:
			if path, ok := data.(string); ok {
				// Prefer Manager.OnExternalUpdate so cross-process file
				// writes (sybra-cli inside an agent worktree) flow
				// through the same status-change hook as in-process
				// updates. Falls back to a bare cache invalidate if the
				// Manager has not been wired yet (degraded-init path).
				if a.tasks != nil {
					a.tasks.OnExternalUpdate(path)
				} else {
					store.InvalidatePath(path)
				}
			}
		}
		a.emit(event, data)
	}
	a.initBgops(emit)

	a.emitDegradedWarnings(emit)
	a.tasks = task.NewManager(store, task.EmitterFunc(emit))
	a.initStatusHook()
	a.notifier = notification.New(emit)
	a.notifier.SetDesktop(a.cfg.Notification.Desktop)
	a.agents = agent.NewManager(ctx, emit, a.logger, a.logDir)
	a.agents.SetDefaultProvider(a.cfg.Agent.Provider)
	a.initProviderHealth(ctx, emit)

	a.prTracker = github.NewIssueTracker(30 * time.Minute)

	// Initialize domain services (dependency order: worktrees → agentOrch → reviewer, workflow)
	a.worktrees = worktree.New(worktree.Config{
		WorktreesDir:     a.worktreesDir,
		Projects:         a.projects,
		Tasks:            a.tasks,
		Logger:           a.logger,
		LogsDir:          a.logDir,
		PRBranchResolver: github.FetchPRBranch,
		AgentChecker:     a.agents.HasRunningAgentForTask,
	})
	a.sandboxes = sandbox.NewManager(filepath.Join(config.HomeDir(), "sandboxes"), a.logger)
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
	a.initTriage()
	issuesFetcher := a.initIssuesFetcher(emit)
	a.wireServices(emit)

	a.syncSkills()
	a.runStartupCleanup()
	a.RegisterSpotlightHotkey()
	a.startBackgroundServices(ctx, emit, issuesFetcher)
	a.logAutomationsSummary()
	a.logger.Info("app.started")
	return nil
}

func (a *App) initBgops(emit func(string, any)) {
	a.bgops = bgop.NewTracker(emit, filepath.Join(config.HomeDir(), "bgops.json"))
	a.bgops.LoadFromDisk()
}

func (a *App) startBackgroundServices(
	ctx context.Context,
	emit func(string, any),
	issuesFetcher *poll.IssuesFetcher,
) {
	a.wg.Go(func() { a.orchestratorLoop(ctx) })

	wdog := watchdog.New(a.agents, a.tasks, a.logger, emit, &a.wg)
	a.wg.Go(func() { wdog.Run(ctx) })

	hcheck := health.New(a.cfg.AuditDir(), a.tasks, config.HomeDir(), a.logger, emit)
	a.wg.Go(func() { hcheck.Run(ctx) })

	a.startMonitorService(ctx, emit)
	a.startSelfMonitorService(ctx, emit)

	a.startPollHub(ctx, issuesFetcher)
	a.startTodoistLoop(ctx)
	a.registerMetricsObservers()
}

// registerMetricsObservers wires the OTel observable gauge callbacks to
// live subsystem state. No-op when metrics are disabled — the metrics
// package holds nil callbacks and the observe() loop returns cheaply.
func (a *App) registerMetricsObservers() {
	metrics.RegisterTasksByStatus(func() map[string]int64 {
		tasks, err := a.tasks.List()
		if err != nil {
			return nil
		}
		out := make(map[string]int64, len(task.AllStatuses()))
		for _, s := range task.AllStatuses() {
			out[string(s)] = 0
		}
		for i := range tasks {
			out[string(tasks[i].Status)]++
		}
		return out
	})
	metrics.RegisterAgentsActive(func() map[string]int64 {
		snapshot := a.agents.ListAgents()
		out := map[string]int64{
			string(agent.StateIdle):    0,
			string(agent.StateRunning): 0,
			string(agent.StatePaused):  0,
			string(agent.StateStopped): 0,
		}
		for _, ag := range snapshot {
			out[string(ag.GetState())]++
		}
		return out
	})
	if a.renovateHandler != nil {
		metrics.RegisterRenovatePRsFetched(a.renovateHandler.LastFetchedCount)
	}
	if a.providerHealth != nil {
		metrics.RegisterProviderHealth(func() map[string]int64 {
			out := make(map[string]int64, 2)
			for name, s := range a.providerHealth.Snapshot() {
				if s.Healthy {
					out[name] = 1
				} else {
					out[name] = 0
				}
			}
			return out
		})
	}
}

// startMonitorService wires the in-process monitor loop when enabled. Does
// nothing when cfg.Monitor.Enabled is false, which keeps the legacy
// /sybra-monitor /loop flow intact for rollout.
func (a *App) startMonitorService(ctx context.Context, emit func(string, any)) {
	if !a.cfg.Monitor.Enabled {
		return
	}
	disp := monitor.NewAgentDispatcher(monitor.AgentDispatcherDeps{
		Agents: a.agents,
		Tasks:  a.tasks,
		WorktreePath: func(t task.Task) (string, bool) {
			if a.worktrees == nil {
				return "", false
			}
			if !a.worktrees.Exists(t) {
				return "", false
			}
			return a.worktrees.PathFor(t), true
		},
		RepoDir:   a.repoDir,
		Model:     a.cfg.Monitor.Model,
		IssueRepo: a.cfg.Monitor.IssueRepo,
	})
	svc := monitor.NewService(monitor.Deps{
		Cfg:        a.cfg.Monitor,
		Tasks:      a.tasks,
		Audit:      monitor.AuditDirReader(a.cfg.AuditDir()),
		Agents:     a.agents,
		Dispatcher: disp,
		Sink:       monitor.NewGHIssueSink(a.cfg.Monitor.IssueLabel, a.cfg.Monitor.IssueRepo),
		Emit:       emit,
		Logger:     a.logger,
		AllowsProject: func(projectID string) bool {
			if projectID == "" {
				return true
			}
			p, err := a.projects.Get(projectID)
			if err != nil {
				return true
			}
			return a.allowsProjectType(p.Type)
		},
	})
	a.monitorSvc = svc
	a.wg.Go(func() { svc.Run(ctx) })
}

// startSelfMonitorService wires the in-process deep-analysis loop that ticks
// every cfg.SelfMonitor.IntervalHours, distills agent logs via loganalyzer,
// and persists a Report to ~/.sybra/selfmonitor/last-report.json. Does
// nothing when cfg.SelfMonitor.Enabled is false, matching the
// startMonitorService gating pattern.
func (a *App) startSelfMonitorService(ctx context.Context, emit func(string, any)) {
	if !a.cfg.SelfMonitor.Enabled {
		return
	}
	ledger, err := selfmonitor.Open(config.SelfMonitorLedgerPath())
	if err != nil {
		a.logger.Error("selfmonitor.ledger_open", "err", err)
		return
	}
	svc := selfmonitor.NewService(selfmonitor.Deps{
		Cfg:            a.cfg.SelfMonitor,
		Tasks:          a.tasks,
		Health:         selfmonitor.DiskHealthReader{Path: config.HealthReportPath()},
		Ledger:         ledger,
		LogsDir:        a.cfg.Logging.Dir,
		LastReportPath: config.SelfMonitorLastReportPath(),
		Emit:           emit,
		Logger:         a.logger,
		AllowsProject: func(projectID string) bool {
			if projectID == "" {
				return true
			}
			p, err := a.projects.Get(projectID)
			if err != nil {
				return true
			}
			return a.allowsProjectType(p.Type)
		},
		Judge: &selfmonitor.ClaudeJudge{
			Model:  a.cfg.SelfMonitor.JudgeModel,
			Logger: a.logger,
		},
		Actor: &selfmonitor.Actor{
			Tasks:  a.tasks,
			DryRun: a.cfg.SelfMonitor.DryRun,
			Logger: a.logger,
		},
		ProviderGate: a.providerHealth,
	})
	a.selfMonitorSvc = svc
	a.wg.Go(func() { svc.Run(ctx) })
}

// GetMonitorReport returns the most recent finished report from the
// in-process monitor service. Ready is false until the first tick completes;
// the frontend should show an empty state in that window. Enabled mirrors
// cfg.Monitor.Enabled so the page can hide the panel entirely on opt-out.
func (a *App) GetMonitorReport() MonitorReportBinding {
	if a.monitorSvc == nil {
		return MonitorReportBinding{Enabled: false}
	}
	r, ok := a.monitorSvc.LastReport()
	return MonitorReportBinding{Enabled: true, Ready: ok, Report: r}
}

// MonitorReportBinding is the Wails-friendly envelope for the latest
// monitor report. Keeping the struct here (rather than in internal/monitor)
// avoids the frontend bindings needing to handle a `monitor.Report | null`
// union — Enabled/Ready flags say whether Report is populated.
type MonitorReportBinding struct {
	Enabled bool           `json:"enabled"`
	Ready   bool           `json:"ready"`
	Report  monitor.Report `json:"report"`
}

// startPollHub registers all enabled poll handlers and starts the hub.
func (a *App) startPollHub(ctx context.Context, issuesFetcher *poll.IssuesFetcher) {
	hub := poll.NewHub()
	hub.Register(a.reviewer, 10*time.Second)
	if issuesFetcher != nil {
		hub.Register(issuesFetcher, 20*time.Second)
	}
	if a.renovateHandler != nil {
		hub.Register(a.renovateHandler, 15*time.Second)
	}
	if a.triageHandler != nil {
		hub.Register(a.triageHandler, 30*time.Second)
	}
	hub.Start(ctx, &a.wg, a.logger)
}

// allowsProjectType reports whether project-scoped automations on this machine
// should act on the given project type. Used to route automation work between
// instances (e.g., pet projects on the server, work projects on the laptop).
func (a *App) allowsProjectType(t project.ProjectType) bool {
	return a.cfg.AllowsProjectType(string(t))
}

// initIssuesFetcher constructs the GitHub Issues fetcher if enabled, returning
// nil otherwise. Kept separate so Startup stays under the funlen limit.
func (a *App) initIssuesFetcher(emit func(string, any)) *poll.IssuesFetcher {
	if !a.cfg.GitHub.Enabled {
		a.logger.Info("github.disabled")
		return nil
	}
	return poll.NewIssuesFetcher(a.tasks, a.projects, emit, a.logger, a.allowsProjectType)
}

// logAutomationsSummary logs a one-line snapshot of which automations this
// machine runs. Useful when comparing two instances side by side.
func (a *App) logAutomationsSummary() {
	loopAgentsEnabled := 0
	if a.loopAgents != nil {
		if las, err := a.loopAgents.List(); err == nil {
			for i := range las {
				if las[i].Enabled {
					loopAgentsEnabled++
				}
			}
		}
	}
	projectTypes := a.cfg.ProjectTypes
	if len(projectTypes) == 0 {
		projectTypes = []string{"*"}
	}
	a.logger.Info("app.automations",
		"todoist", a.cfg.Todoist.Enabled && a.cfg.Todoist.APIToken != "",
		"github", a.cfg.GitHub.Enabled,
		"renovate", a.cfg.Renovate.Enabled,
		"triage", a.cfg.Triage.Enabled,
		"project_types", projectTypes,
		"loop_agents_enabled", loopAgentsEnabled,
	)
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

// initProviderHealth constructs the provider health checker, wires it into
// the agent manager as a gate, and starts its background probe loop. When
// providers.health_check.enabled=false the checker is skipped entirely and
// the manager runs with a nil gate (no blocking).
func (a *App) initProviderHealth(ctx context.Context, emit func(string, any)) {
	if !a.cfg.Providers.HealthCheck.Enabled {
		a.logger.Info("provider.health.disabled")
		return
	}
	pc := provider.New(provider.Config{
		Interval:         time.Duration(a.cfg.Providers.HealthCheck.IntervalSeconds) * time.Second,
		ClaudeEnabled:    a.cfg.Providers.Claude.Enabled,
		CodexEnabled:     a.cfg.Providers.Codex.Enabled,
		AutoFailover:     a.cfg.Providers.AutoFailover,
		ClaudeRLCooldown: time.Duration(a.cfg.Providers.Claude.RateLimitCooldownSeconds) * time.Second,
		CodexRLCooldown:  time.Duration(a.cfg.Providers.Codex.RateLimitCooldownSeconds) * time.Second,
	}, emit, a.logger)
	a.providerHealth = pc
	a.agents.SetHealthGate(pc)
	a.wg.Go(func() { pc.Run(ctx) })
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
	outputs := ag.Output()
	for i := range outputs {
		if outputs[i].Type == "result" {
			resultContent = outputs[i].Content
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
		"log_file":   ag.LogPath,
	})

	// Loop agents run without a TaskID — let the scheduler record cost
	// before the early return below kicks in.
	if a.loopSched != nil {
		a.loopSched.OnAgentComplete(ag)
	}

	// Orchestrator brain agents run with TaskID="" (rooted at ~/.sybra,
	// no parent task). Calling UpdateRun / HandleAgentComplete / Get with
	// an empty ID joins to ".sybra/tasks/.md" and crashes the handler.
	if ag.TaskID == "" {
		return
	}

	// Persist run result to task file.
	truncated := resultContent
	if len(truncated) > maxResultLen {
		truncated = truncated[:maxResultLen] + "\n... (truncated)"
	}
	if err := a.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":      string(state),
		"cost_usd":   cost,
		"result":     truncated,
		"log_file":   ag.LogPath,
		"session_id": ag.GetSessionID(),
	}); err != nil {
		a.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

	// Advance workflow.
	if a.workflowEngine != nil {
		a.workflowEngine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   resultContent,
			Provider: ag.Provider,
			Success:  exitErr == nil,
		})
	}

	// Worktree and sandbox cleanup for done tasks (after engine advances, so status is final).
	if t, err := a.tasks.Get(ag.TaskID); err == nil && t.Status == task.StatusDone {
		go a.worktrees.Remove(ag.TaskID)
		if a.sandboxes != nil {
			go a.sandboxes.Stop(ag.TaskID)
		}
	}
}

func (a *App) onWorkflowComplete(info workflow.CompletionInfo) {
	kind := github.PRIssueKind(info.Variables["pr_issue_kind"])
	if kind == "" {
		return
	}
	a.prTracker.ClearCooldown(info.TaskID, kind)
	a.logger.Info("pr-tracker.cooldown-cleared",
		"task_id", info.TaskID, "kind", string(kind),
		"retries", a.prTracker.Retries(info.TaskID, kind))
}

func (a *App) initWorkflowEngine() {
	if os.Getenv("SYBRA_DISABLE_WORKFLOWS") == "1" {
		a.logger.Info("workflow.disabled")
		return
	}
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
		&taskAdapter{tasks: a.tasks, projects: a.projects},
		&agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks, sandboxes: a.sandboxes},
		a.logger,
	)
	a.workflowEngine.SetPRLinker(prLinkerAdapter{})
	a.workflowEngine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetOnComplete(a.onWorkflowComplete)
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
	a.taskSvc.sandboxes = a.sandboxes
	a.taskSvc.wg = &a.wg
	a.taskSvc.logger = a.logger
	a.taskSvc.audit = a.audit
	a.planSvc.engine = a.workflowEngine
	a.planSvc.tasks = a.tasks
	a.planSvc.agents = a.agents
	a.agentSvc.agents = a.agents
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit
	a.agentOrch.sandboxes = a.sandboxes
	a.projectSvc.projects = a.projects
	a.projectSvc.worktrees = a.worktrees
	a.projectSvc.logger = a.logger
	a.projectSvc.notifier = a.notifier
	a.projectSvc.bgops = a.bgops
	a.projectSvc.wg = &a.wg
	a.agentOrch.bgops = a.bgops
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
	a.intgSvc.providerHealth = a.providerHealth
	a.intgSvc.saveConfig = func() error { return a.cfg.Save() }
	a.statsSvc.stats = a.stats
	a.workflowSvc.engine = a.workflowEngine
	a.workflowSvc.store = a.workflowStore
}

func (a *App) Shutdown(_ context.Context) {
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

// cleanStaleRuns marks agent_runs still showing "running" as "stopped" if no
// matching in-memory agent exists. Fixes leftover state from crashes/restarts.
func (a *App) cleanStaleRuns() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		if tasks[i].TaskType == task.TaskTypeChat {
			continue
		}
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

// runStartupCleanup sequences boot-time maintenance in the order that lets
// each step see the output of the previous one: chats first so their
// worktrees show up as orphans to the subsequent sweep; stale run state
// next so restart-stale sees a clean slate.
func (a *App) runStartupCleanup() {
	a.worktrees.RepairAll()
	a.gcOrphanChats()
	a.worktrees.CleanupOrphaned()
	a.cleanStaleRuns()
	a.restartStaleInProgress()
}

// gcOrphanChats deletes any chat-task that no longer has a running agent.
// Chats are ephemeral by design; a stale chat-task is always noise left over
// from a crash or kill. Runs before worktree orphan cleanup so the task
// file is gone by the time the worktree sweeper looks.
func (a *App) gcOrphanChats() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.TaskType != task.TaskTypeChat {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		a.logger.Info("chat.gc.orphan", "task_id", t.ID, "title", t.Title)
		a.worktrees.Remove(t.ID)
		if err := a.tasks.Delete(t.ID); err != nil {
			a.logger.Error("chat.gc.delete", "task_id", t.ID, "err", err)
		}
	}
}

// restartStaleMinAge is the minimum age of the latest agent run before a
// stale in-progress task is eligible for respawn. Protects against dev-mode
// hot-reload loops spawning parallel agents onto the same task.
const restartStaleMinAge = 5 * time.Minute

// restartStaleInProgress recovers in-progress tasks that lost their agent
// due to a crash or restart. Headless tasks are re-dispatched; interactive
// tasks drive the workflow engine forward via recoverStaleInteractive.
func (a *App) restartStaleInProgress() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if t.Status != task.StatusInProgress {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if slices.Contains(t.Tags, "review") {
			continue
		}
		// Tasks with a terminal workflow stuck at in-progress: restart the
		// workflow rather than spawning a bare agent. A bare agent spawn would
		// loop forever (completion callback can't advance a terminal workflow),
		// but restarting the workflow gives the callback a live execution to
		// advance. Skip the remaining stale-restart logic for these.
		if a.workflowEngine != nil && t.Workflow != nil &&
			(t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed) {
			wfID := t.Workflow.WorkflowID
			taskID := t.ID
			a.logger.Info("restart-stale.restart-workflow", "task_id", taskID, "workflow", wfID)
			a.wg.Go(func() {
				if wfErr := a.workflowEngine.StartWorkflow(taskID, wfID); wfErr != nil {
					a.logger.Error("restart-stale.restart-workflow.failed", "task_id", taskID, "err", wfErr)
				}
			})
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
		// Interactive: drive the workflow engine to advance the current step
		// using the stored agent run result — same mechanism as onAgentComplete.
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
				metrics.OrchestratorStaleRestart(err == nil)
				a.restartStaleErr.Log(a.logger, "restart.pr-fix.failed", "pr-fix:"+taskID, err, "task_id", taskID)
			})
		} else {
			mode := t.AgentMode
			prFlag := " --draft"
			if proj, pErr := a.projects.Get(t.ProjectID); pErr == nil && proj.Type == project.ProjectTypePet {
				prFlag = ""
			}
			prompt := "Continue implementing this task. When done, create a PR with `gh pr create" + prFlag + "`."
			a.wg.Go(func() {
				// Restart-stale only ever reaches this branch for headless
				// mode (interactive tasks are handled by recoverStaleInteractive
				// above), so OneShot is irrelevant here — pass false.
				_, err := a.agentOrch.StartAgent(taskID, mode, prompt, false)
				metrics.OrchestratorStaleRestart(err == nil)
				a.restartStaleErr.Log(a.logger, "restart-stale.failed", "stale:"+taskID, err, "task_id", taskID)
			})
		}
	}
}

// recoverStaleInteractive handles interactive in-progress tasks whose agent
// died or disappeared across restarts. Marks the last agent run as
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
		AgentID:  lr.AgentID,
		Provider: lr.Provider,
		Success:  true,
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

// StartChat creates a new interactive chat bound to projectID using the
// requested provider ("claude" or "codex"). Each chat gets a dedicated
// local-only worktree that is cleaned up when StopChat is called.
func (a *App) StartChat(projectID, providerName, prompt string) (*agent.Agent, error) {
	return a.agentOrch.StartChat(projectID, providerName, prompt)
}

// StopChat stops a chat agent, deletes its synthetic task, and removes its
// worktree. Refuses to operate on agents that are not bound to a chat task
// so the UI cannot accidentally delete a real task.
func (a *App) StopChat(agentID string) error {
	ag, err := a.agents.GetAgent(agentID)
	if err != nil {
		return err
	}
	if ag.TaskID == "" {
		return fmt.Errorf("agent %s is not bound to a task", agentID)
	}
	t, err := a.tasks.Get(ag.TaskID)
	if err != nil {
		return fmt.Errorf("lookup chat task: %w", err)
	}
	if t.TaskType != task.TaskTypeChat {
		return fmt.Errorf("agent %s is not a chat (task_type=%s)", agentID, t.TaskType)
	}
	return a.taskSvc.DeleteTask(t.ID)
}

// seedDefaultLoopAgents creates the built-in sybra-self-monitor loop on
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
	const name = "sybra-self-monitor"
	if _, ok := a.loopAgents.FindByName(name); ok {
		return
	}
	created, err := a.loopAgents.Create(loopagent.LoopAgent{
		Name:         name,
		Prompt:       "/sybra-self-monitor",
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
		} else {
			repoDir = cwd
			a.logger.Info("skills.sync.fallback_cwd", "dir", cwd)
		}
	}

	skillsSrc := filepath.Join(repoDir, ".claude", "skills")

	// When the filesystem source doesn't exist (e.g. Docker deployments where
	// the repo is absent), fall back to the embedded bundle so skills are
	// always available in ~/.claude/skills/ and ~/.codex/skills/.
	if _, err := os.Stat(skillsSrc); os.IsNotExist(err) && a.skillsFS != nil {
		a.logger.Info("skills.sync.embedded_fallback", "dst", a.skillsDir)
		a.syncFSDir(a.skillsFS, "data", a.skillsDir)
		if userHome, err2 := os.UserHomeDir(); err2 == nil {
			a.syncFSDir(a.skillsFS, "data", filepath.Join(userHome, ".claude", "skills"))
			a.syncFSToCodexDir(a.skillsFS, "data", filepath.Join(userHome, ".codex", "skills"))
		}
		// orchestrator CLAUDE.md is not in the embedded bundle; skip it.
		a.logger.Info("skills.sync.done")
		return
	}

	a.logger.Info("skills.sync.start", "src", repoDir, "dst", a.skillsDir)

	a.syncDir(skillsSrc, a.skillsDir)

	// Also sync to the system Claude Code and Codex skills dirs so headless
	// agents launched via `claude -p` or `codex exec` can discover skills.
	if userHome, err := os.UserHomeDir(); err == nil {
		a.syncDir(skillsSrc, filepath.Join(userHome, ".claude", "skills"))
		a.syncToCodexDir(skillsSrc, filepath.Join(userHome, ".codex", "skills"))
	}

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

// syncFSDir copies .md files from srcDir inside fsys to the dst filesystem path.
// It mirrors syncDir but reads from an fs.FS instead of the host filesystem.
func (a *App) syncFSDir(fsys fs.FS, srcDir, dst string) {
	entries, err := fs.ReadDir(fsys, srcDir)
	if err != nil {
		a.logger.Debug("sync.fs.skip", "src", srcDir, "reason", err)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		a.logger.Error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		dstPath := filepath.Join(filepath.Clean(dst), e.Name())
		if !strings.HasPrefix(dstPath+string(filepath.Separator), cleanDst) {
			a.logger.Warn("sync.skip.traversal", "name", e.Name())
			continue
		}
		data, err := fs.ReadFile(fsys, srcDir+"/"+e.Name())
		if err != nil {
			a.logger.Warn("sync.fs.read.fail", "name", e.Name(), "err", err)
			continue
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			a.logger.Error("sync.write", "dst", dstPath, "err", err)
			continue
		}
		a.logger.Info("sync.copied", "file", e.Name())
		srcNames[e.Name()] = struct{}{}
	}

	// Remove orphan .md files in dst that are not in the embedded bundle.
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

// syncToCodexDir reads flat .md skill files from src and writes each one as
// dst/<name>/SKILL.md — the subdirectory layout expected by the Codex CLI.
// Orphan skill subdirs (present in dst but absent from src) are removed.
func (a *App) syncToCodexDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		a.logger.Debug("sync.codex.skip", "src", src, "reason", err)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		a.logger.Error("sync.codex.mkdir", "dst", dst, "err", err)
		return
	}
	cleanSrc := filepath.Clean(src) + string(filepath.Separator)
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		srcPath := filepath.Join(filepath.Clean(src), e.Name())
		if !strings.HasPrefix(srcPath+string(filepath.Separator), cleanSrc) {
			a.logger.Warn("sync.codex.skip.traversal", "name", e.Name())
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		skillDir := filepath.Join(filepath.Clean(dst), name)
		if !strings.HasPrefix(skillDir+string(filepath.Separator), cleanDst) {
			a.logger.Warn("sync.codex.skip.traversal", "name", e.Name())
			continue
		}
		dstPath := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(srcPath)
		if err != nil {
			a.logger.Warn("sync.codex.read.fail", "name", e.Name(), "err", err)
			continue
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			a.logger.Error("sync.codex.mkdir.skill", "dir", skillDir, "err", err)
			continue
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			a.logger.Error("sync.codex.write", "dst", dstPath, "err", err)
			continue
		}
		a.logger.Info("sync.codex.copied", "skill", name)
		srcNames[name] = struct{}{}
	}

	// Remove orphan skill subdirs that no longer exist in src.
	dstEntries, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range dstEntries {
		if !e.IsDir() {
			continue
		}
		if _, ok := srcNames[e.Name()]; ok {
			continue
		}
		// Only remove dirs that contain a SKILL.md we could have written.
		skillMD := filepath.Join(filepath.Clean(dst), e.Name(), "SKILL.md")
		if !strings.HasPrefix(skillMD, cleanDst) {
			continue
		}
		if _, statErr := os.Stat(skillMD); statErr != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(filepath.Clean(dst), e.Name())); err != nil {
			a.logger.Warn("sync.codex.orphan.remove.fail", "skill", e.Name(), "err", err)
		} else {
			a.logger.Info("sync.codex.orphan.removed", "skill", e.Name())
		}
	}
}

// syncFSToCodexDir mirrors syncToCodexDir but reads source files from an fs.FS.
func (a *App) syncFSToCodexDir(fsys fs.FS, srcDir, dst string) {
	entries, err := fs.ReadDir(fsys, srcDir)
	if err != nil {
		a.logger.Debug("sync.codex.fs.skip", "src", srcDir, "reason", err)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		a.logger.Error("sync.codex.mkdir", "dst", dst, "err", err)
		return
	}
	cleanDst := filepath.Clean(dst) + string(filepath.Separator)

	srcNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		skillDir := filepath.Join(filepath.Clean(dst), name)
		if !strings.HasPrefix(skillDir+string(filepath.Separator), cleanDst) {
			a.logger.Warn("sync.codex.skip.traversal", "name", e.Name())
			continue
		}
		dstPath := filepath.Join(skillDir, "SKILL.md")
		data, err := fs.ReadFile(fsys, srcDir+"/"+e.Name())
		if err != nil {
			a.logger.Warn("sync.codex.fs.read.fail", "name", e.Name(), "err", err)
			continue
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			a.logger.Error("sync.codex.mkdir.skill", "dir", skillDir, "err", err)
			continue
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			a.logger.Error("sync.codex.write", "dst", dstPath, "err", err)
			continue
		}
		a.logger.Info("sync.codex.copied", "skill", name)
		srcNames[name] = struct{}{}
	}

	// Remove orphan skill subdirs not in the embedded bundle.
	dstEntries, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range dstEntries {
		if !e.IsDir() {
			continue
		}
		if _, ok := srcNames[e.Name()]; ok {
			continue
		}
		skillMD := filepath.Join(filepath.Clean(dst), e.Name(), "SKILL.md")
		if !strings.HasPrefix(skillMD, cleanDst) {
			continue
		}
		if _, statErr := os.Stat(skillMD); statErr != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(filepath.Clean(dst), e.Name())); err != nil {
			a.logger.Warn("sync.codex.orphan.remove.fail", "skill", e.Name(), "err", err)
		} else {
			a.logger.Info("sync.codex.orphan.removed", "skill", e.Name())
		}
	}
}

// ListBackgroundOps returns active and recently-completed background operations.
func (a *App) ListBackgroundOps() []bgop.Operation {
	if a.bgops == nil {
		return nil
	}
	return a.bgops.List()
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

// BindTargets returns the objects to bind for Wails IPC.
func (a *App) BindTargets() []any {
	return []any{
		a,
		a.taskSvc, a.planSvc, a.agentSvc, a.orchSvc,
		a.projectSvc, a.loopAgentSvc, a.configSvc, a.intgSvc,
		a.statsSvc, a.reviewSvc, a.workflowSvc, a.infoSvc,
	}
}

// Context returns the app's running context.
func (a *App) Context() context.Context { return a.ctx }
