package sybra

// Dependency graph (startup order):
//
//	config → audit/stats → task.Store → project.Store → loopagent.Store
//	→ emit/bgops → task.Manager → limits/approval → agent.Manager → providerHealth
//	→ worktrees → sandboxes → agentOrch → reviewer → workflowEngine
//	→ wireServices → [LifecycleManager: StartManagers → StartPollers → StartWatchers]

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/confighot"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/selfmonitor"
	"github.com/Automaat/sybra/internal/spotlight"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

type App struct {
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	tasks             *task.Manager
	projects          *project.Store
	loopAgents        *loopagent.Store
	loopSched         *loopagent.Scheduler
	agents            *agent.Manager
	watcher           *watcher.Watcher
	configWatcher     *confighot.Watcher
	notifier          *notification.Emitter
	audit             *audit.Logger
	artifacts         *artifact.Store
	experience        *experience.Store
	learning          *learning.Store
	stats             *stats.Store
	limits            *limits.Store
	tasksDir          string
	skillsDir         string
	repoDir           string
	worktreesDir      string
	logger            *slog.Logger
	logDir            string
	auditDir          string
	prTracker         *github.IssueTracker
	providerHealth    *provider.Checker
	worktrees         *worktree.Manager
	sandboxes         *sandbox.Manager
	monitorSvc        *monitor.Service
	selfMonitorSvc    *selfmonitor.Service
	evaluationSvc     *evaluation.Service
	learningDigestSvc *learning.Service
	agentOrch         *AgentOrchestrator
	reviewer          *ReviewHandler
	workflowEngine    *workflow.Engine
	workflowStore     *workflow.Store
	todoist           *todoistCoordinator
	renovate          *renovateCoordinator
	promptLab         *promptLabCoordinator
	triage            *triageCoordinator
	humanReview       *humanReviewHandler
	cfg               *config.Config
	logLevel          *slog.LevelVar
	emit              func(string, any)
	emitFactory       func(context.Context) func(string, any)
	openBrowser       func(string)
	requestRestart    func()
	restartStaleErr   *logging.ErrorThrottle
	// dispatchNudge wakes the orchestrator dispatch pass on demand (e.g. on a
	// status change) so a freshly-ready task isn't left idle until the next
	// fast tick. Buffered, size 1, coalescing — see nudgeDispatch.
	dispatchNudge   chan struct{}
	recovery        *recovery.Recovery
	agentCompletion *AgentCompletionHandler
	// umbrellaCloseIssue closes the umbrella GitHub issue on full roll-up.
	// nil defaults to github.CloseIssue; overridden in tests.
	umbrellaCloseIssue func(repo string, number int, comment string) error

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
	browserSvc   *BrowserService
	learningSvc  *LearningService
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

// WithBrowserOpener injects the closure that opens a URL in an in-app webview
// window. Supplied by the desktop entrypoint (darwin); left unset on the
// headless server, where BrowserService.Open reports the feature unavailable.
func WithBrowserOpener(fn func(string)) Option {
	return func(a *App) { a.openBrowser = fn }
}

// WithRestartRequest injects the host-specific graceful-restart hook. Desktop
// uses Wails Quit; the HTTP server cancels its root context.
func WithRestartRequest(fn func()) Option {
	return func(a *App) { a.requestRestart = fn }
}

// WithSkillsFS sets an embedded FS used as a fallback source for skill files
// when the repository's .claude/skills/ directory is not present on disk.
// Typically populated from the internal/skills package in the server binary.
func WithSkillsFS(skillsFS fs.FS) Option {
	return func(a *App) { a.skillsFS = skillsFS }
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
		dispatchNudge:   make(chan struct{}, 1),
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
	a.browserSvc = &BrowserService{}
	a.learningSvc = &LearningService{}
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
				// A task file appearing outside GUI CreateTask (e.g. via
				// sybra-cli) must still get its task.created workflow, or it
				// sits inert in todo with nothing to dispatch it.
				if event == events.TaskCreated {
					a.maybeStartWorkflowForExternalTask(path)
				}
			}
		}
		a.emit(event, data)
	}
	a.initBgops(emit)

	a.emitDegradedWarnings(emit)
	a.tasks = task.NewManager(store, task.EmitterFunc(emit))
	a.initStatusHook()
	a.initLocalStores()
	a.notifier = notification.New(emit)
	a.notifier.SetDesktop(a.cfg.Notification.Desktop)
	a.initLimits()
	if err := a.initAgentManager(ctx, emit); err != nil {
		return err
	}
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
	a.reviewer = newReviewHandler(a.tasks, a.projects, a.agents, a.audit, a.logger, a.prTracker, emit, a.worktrees, a.renovatePRsForMonitor, a.cfg, a.experience)

	a.initWorkflowEngine()

	a.initAgentConfig()

	a.initLoopScheduler(ctx, emit)
	a.initFileWatcher(ctx, emit)

	issuesFetcher := a.initAutomations(emit)
	a.wireServices(emit)

	a.syncSkillsBundle()
	a.recovery = a.newRecovery()
	a.recovery.RunStartupCleanup()
	a.RegisterSpotlightHotkey()

	lm := newLifecycleManager(a)
	lm.StartManagers(ctx, emit)
	lm.StartPollers(ctx, emit, issuesFetcher)
	lm.StartWatchers(ctx)

	a.logAutomationsSummary()
	a.logger.Info("app.started")
	return nil
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

// GetEvaluationReport returns the latest fleet scorecard, computing one on
// demand when the background ticker hasn't run (or is disabled).
func (a *App) GetEvaluationReport() evaluation.Report {
	if a.evaluationSvc == nil {
		return evaluation.Report{}
	}
	return a.evaluationSvc.GetEvaluationReport()
}

// GetLifecyclePhases returns the per-phase lifecycle-duration breakdown for
// tasks that landed in the evaluation window — where end-to-end time is spent
// (planning vs implementing vs testing vs review vs waiting).
func (a *App) GetLifecyclePhases() evaluation.PhaseReport {
	if a.evaluationSvc == nil {
		return evaluation.PhaseReport{}
	}
	rep, err := a.evaluationSvc.PhaseReport(context.Background())
	if err != nil {
		a.logger.Warn("evaluation.phases.failed", "err", err)
		return evaluation.PhaseReport{}
	}
	return rep
}

// RunLearningDigestNow synchronously generates and persists a fresh Learning
// Digest, returning it or a clear error (insufficient fresh data, a
// malformed/invalid summarizer response, or a persist failure). A failed run
// leaves the previously-stored digest intact — see internal/learning.
func (a *App) RunLearningDigestNow(ctx context.Context) (learning.Digest, error) {
	if a.learningDigestSvc == nil {
		return learning.Digest{}, fmt.Errorf("learning digest service unavailable")
	}
	return a.learningDigestSvc.RunNow(ctx)
}

// GetLearningDigestStatus returns the Learning Digest service's current
// state: whether it is enabled, the most recently persisted digest, and an
// estimate of the next scheduled run.
func (a *App) GetLearningDigestStatus() learning.Status {
	if a.learningDigestSvc == nil {
		return learning.Status{}
	}
	return a.learningDigestSvc.Status()
}

// fleetWorkBlocklist returns the union of {id, owner, repo, url} over every
// registered work-typed project, for scrubbing fleet-wide artifacts (e.g. a
// Learning Digest, which aggregates across all projects rather than a single
// task) that cannot use the narrower per-task workScrubContextForTask.
func (a *App) fleetWorkBlocklist() []string {
	if a.projects == nil {
		return nil
	}
	projs, err := a.projects.List()
	if err != nil {
		return nil
	}
	var bl []string
	for i := range projs {
		if projs[i].Type != project.ProjectTypeWork {
			continue
		}
		bl = append(bl, projs[i].ID, projs[i].Owner, projs[i].Repo)
		if projs[i].URL != "" {
			bl = append(bl, projs[i].URL)
		}
	}
	return bl
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
	if a.agents != nil {
		a.agents.Shutdown()
	}
	if a.audit != nil {
		_ = a.audit.Close()
	}
	a.logger.Info("app.stopped")
}

// StartAgent delegates to AgentOrchestrator and is exposed as a Wails-bound method.
// User-triggered starts are never one-shot — that flag is reserved for workflow
// steps that expect a single turn.
func (a *App) StartAgent(taskID, mode, prompt string, includeTaskDescription bool) (*agent.Agent, error) {
	return a.agentOrch.StartAgent(taskID, mode, prompt, includeTaskDescription, false)
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

// Context returns the app's running context.
func (a *App) Context() context.Context { return a.ctx }
