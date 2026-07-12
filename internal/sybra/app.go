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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/confighot"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/fsutil"
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
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/sybra/completion"
	"github.com/Automaat/sybra/internal/sybra/review"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/tasksnapshot"
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
	agentQueue        *agentqueue.Queue
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
	homeUnlock        func() error
	monitorSvc        *monitor.Service
	selfMonitorSvc    *selfmonitor.Service
	evaluationSvc     *evaluation.Service
	learningDigestSvc *learning.Service
	agentOrch         *agentorch.Orchestrator
	reviewer          *review.Handler
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
	snapshotter     *tasksnapshot.Snapshotter
	agentCompletion *completion.Handler
	// umbrellaCloseIssue closes the umbrella GitHub issue on full roll-up.
	// nil defaults to github.CloseIssue; overridden in tests.
	umbrellaCloseIssue func(repo string, number int, comment string) error

	// umbrellaRecoveryMu guards umbrellaRecoveryInFlight — App-owned recovery
	// coordination state (deliberately not a package-level map) for the
	// degraded-umbrella auto-recovery pass: recoverDegradedUmbrellas
	// single-flights an async RecoverDegraded run per normalized umbrella
	// ref, and releaseUnblockedChildren excludes in-flight refs from release
	// and tracker rollup for that gate tick.
	umbrellaRecoveryMu       sync.Mutex
	umbrellaRecoveryInFlight map[string]bool
	// umbrellaRecoverFn overrides the single recovery attempt run per
	// scheduled tracker. nil defaults to runUmbrellaRecovery; tests override
	// it to exercise scheduling/single-flight without spawning a real
	// planner subprocess.
	umbrellaRecoverFn func(tracker task.Task)
	// taskAgentReleaser overrides releaseTaskAgents for tests. nil uses the
	// live agent-manager-backed implementation.
	taskAgentReleaser func(taskID string)

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
	promptLabSvc *PromptLabService

	// HTTP-only services. QueueService must stay out of V3Services().
	queueSvc *QueueService
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
		tasksDir:                 cfg.TasksDir,
		skillsDir:                cfg.SkillsDir,
		repoDir:                  cfg.RepoDir,
		worktreesDir:             cfg.WorktreesDir,
		logger:                   logger,
		logDir:                   cfg.Logging.Dir,
		auditDir:                 cfg.AuditDir(),
		cfg:                      cfg,
		logLevel:                 logLevel,
		restartStaleErr:          logging.NewErrorThrottle(),
		dispatchNudge:            make(chan struct{}, 1),
		umbrellaRecoveryInFlight: make(map[string]bool),
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
	a.promptLabSvc = &PromptLabService{}
	a.queueSvc = &QueueService{}
	for _, o := range opts {
		o(a)
	}
	return a
}

// acquireHomeLock takes an exclusive, non-blocking flock on
// <SYBRA_HOME>/sybra.lock, held for the process lifetime, so a second Sybra
// instance (desktop, sybra-server, or an app-under-test spawned by a
// test-runner agent) pointed at the same home fails fast instead of racing
// the first over task files, in-memory agent state, and pollers — the
// dual-instance incident this guards against saw a second instance reattach
// to the production instance's live agents and corrupt its completion
// bookkeeping. The unlock is released in Shutdown; if Shutdown is never
// called (process killed, os.Exit on a Startup error), the OS releases the
// flock when the process's file descriptors close.
func (a *App) acquireHomeLock() error {
	unlock, err := fsutil.TryLockPath(filepath.Join(config.HomeDir(), "sybra.lock"))
	if err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			return fmt.Errorf("another Sybra instance is already running against %s (%w) — stop it first, or point this run at a different SYBRA_HOME", config.HomeDir(), err)
		}
		if errors.Is(err, fsutil.ErrLockUnsupported) {
			a.logger.Warn("app.home_lock.unsupported", "home", config.HomeDir(), "err", err)
			return nil
		}
		return fmt.Errorf("acquire home lock: %w", err)
	}
	a.homeUnlock = unlock
	return nil
}

func (a *App) startLifecycle(ctx context.Context, emit func(string, any)) {
	a.initLoopScheduler(ctx, emit)
	a.initFileWatcher(ctx, emit)

	issuesFetcher := a.initAutomations(emit)
	a.wireServices(emit)

	// syncSkillsBundle's deep diagnostic logging uses context.Background()
	// intentionally (see skillsync.Syncer.log) — not a cancellation bug.
	a.syncSkillsBundle() //nolint:contextcheck // plain diagnostic logging inside skillsync, see its log() comment
	a.snapshotter = tasksnapshot.New(config.TaskSnapshotGitDir(), a.tasksDir, time.Duration(a.cfg.DefaultTaskSnapshotInterval())*time.Second, a.logger)
	// EnsureRepo must run before RunStartupCleanup: the startup trash prune
	// fires CommitBeforePrune, which on a fresh install would otherwise commit
	// into an uninitialized git dir and fail silently. StartManagers'
	// startTaskSnapshotLoop calls EnsureRepo again (idempotent).
	if a.cfg.TaskSnapshotEnabled() {
		a.snapshotter.EnsureRepo(ctx)
	}
	a.recovery = a.newRecovery()
	a.RegisterSpotlightHotkey() //nolint:contextcheck // agent.Manager dispatch chain uses its own m.ctx field, see Startup's contextcheck note

	lm := newLifecycleManager(a)
	lm.StartWatchers(ctx)

	a.wg.Go(func() {
		a.recovery.RunStartupCleanup(ctx)
		lm.StartManagers(ctx, emit)
		lm.StartPollers(ctx, emit, issuesFetcher)
	})
}

func (a *App) cleanupFailedStartup() {
	if a.cancel != nil {
		a.cancel()
	}
	if a.homeUnlock != nil {
		if err := a.homeUnlock(); err != nil {
			a.logger.Warn("app.home_unlock.failed", "err", err)
		}
		a.homeUnlock = nil
	}
}

// Startup initializes all subsystems. Returns an error if a critical subsystem
// fails; callers (Wails OnStartup, HTTP server main) handle the error.
// contextcheck note: several call chains below (emit's task.created dispatch,
// initStatusHook, initLimits, RegisterSpotlightHotkey) eventually reach a
// consumer — workflow.Engine's execShell, App.ctx-derived backfill, or
// agent.Manager's dispatch chain — that already derives its context from
// this same Startup(ctx) via a long-lived field (Engine.SetContext(a.ctx),
// a.ctx itself, or agent.Manager's own m.ctx) rather than an explicit
// parameter threaded through every intermediate call. Each field is bound
// exactly once from this ctx, so cancellation still propagates correctly;
// contextcheck cannot see field-based propagation and flags the gap between
// this ctx and the eventual consumer. Re-plumbing ctx as an explicit
// parameter through the entire event/dispatch/workflow fan-out these chains
// pass through is out of scope for this pass — nolint annotations below
// point back to this comment.
func (a *App) Startup(ctx context.Context) error {
	if err := a.acquireHomeLock(); err != nil {
		return err
	}
	started := false
	defer func() {
		if started {
			return
		}
		a.cleanupFailedStartup()
	}()

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
	// This closure's DispatchEvent -> execShell eventually derives its
	// context from workflow.Engine's own e.ctx field (Engine.SetContext),
	// not an explicit parameter threaded through the closure. contextcheck
	// no longer flags this call site (verified with a clean build+lint
	// cache), so no suppression directive is needed here.
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
	a.initStatusHook() //nolint:contextcheck // workflow engine uses its own e.ctx field, see Startup's contextcheck note
	a.initLocalStores()
	a.notifier = notification.New(emit)
	a.notifier.SetDesktop(a.cfg.Notification.Desktop)
	a.initLimits() //nolint:contextcheck // backfill derives from a.ctx directly, see Startup's contextcheck note
	// sandboxes has no dependency on the agent manager and must exist before
	// initAgentManager so ManagerConfig.SandboxHome can be wired at construction.
	a.sandboxes = sandbox.NewManager(filepath.Join(config.HomeDir(), "sandboxes"), a.logger)
	if err := a.initAgentManager(ctx, emit); err != nil {
		return err
	}
	a.initProviderHealth(ctx, emit)

	a.prTracker = github.NewIssueTracker(30 * time.Minute)

	// Bound how often FetchOrigin does a real network fetch per bare clone —
	// fix/review/pr-fix prepares call it unconditionally on every dispatch, so
	// without a TTL a tight cluster of dispatches against one repo (hundreds
	// of pr-fix runs/month, see issue #1527) pays for a full-branch fetch on
	// every single one.
	project.FetchTTL = 60 * time.Second
	// Initialize domain services (dependency order: worktrees → agentOrch → reviewer, workflow)
	a.worktrees = worktree.New(worktree.Config{
		WorktreesDir:     a.worktreesDir,
		Projects:         a.projects,
		Tasks:            a.tasks,
		Logger:           a.logger,
		LogsDir:          a.logDir,
		PRBranchResolver: github.FetchPRBranch,
		AgentChecker:     a.agents.HasRunningAgentForTask,
		LiveAgentChecker: a.agents.HasLiveRegisteredAgentForTask,
	})
	a.agentOrch = agentorch.New(a.tasks, a.projects, a.agents, a.audit, a.logger, a.worktrees, a.cfg)
	a.agentOrch.SetContext(ctx)
	a.reviewer = review.New(a.tasks, a.projects, a.agents, a.audit, a.logger, a.prTracker, emit, a.worktrees, a.renovatePRsForMonitor, a.cfg, a.experience)

	a.initWorkflowEngine()

	a.initAgentConfig()

	a.startLifecycle(ctx, emit)

	a.logAutomationsSummary()
	a.logger.Info("app.started")
	started = true
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
// Fails closed: a projects.List() error is returned rather than swallowed,
// so callers treat "couldn't build the blocklist" as a hard error instead of
// silently proceeding with an unredacted digest.
func (a *App) fleetWorkBlocklist() ([]string, error) {
	if a.projects == nil {
		return nil, nil
	}
	projs, err := a.projects.List()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
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
	return bl, nil
}

func (a *App) Shutdown(_ context.Context) {
	a.logger.Info("app.stopping")
	if a.loopSched != nil {
		a.loopSched.Stop()
	}
	if a.cancel != nil {
		a.cancel()
	}
	if !waitGroupTimeout(&a.wg, appShutdownWaitGrace) {
		a.logger.Warn("app.shutdown.wait_timeout", "grace", appShutdownWaitGrace, "stacks", a.dumpGoroutineStacks())
	}
	if a.agents != nil {
		a.agents.Shutdown()
	}
	if a.audit != nil {
		_ = a.audit.Close()
	}
	if a.homeUnlock != nil {
		if err := a.homeUnlock(); err != nil {
			a.logger.Warn("app.home_unlock.failed", "err", err)
		}
		a.homeUnlock = nil
	}
	a.logger.Info("app.stopped")
}

const appShutdownWaitGrace = 15 * time.Second

func waitGroupTimeout(wg *sync.WaitGroup, grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

func (a *App) dumpGoroutineStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if a.logDir == "" {
		return "no logDir; " + fmt.Sprintf("%d goroutines", runtime.NumGoroutine())
	}
	path := filepath.Join(a.logDir, "shutdown-stacks.txt")
	if err := os.WriteFile(path, buf[:n], 0o644); err != nil {
		return "dump failed: " + err.Error()
	}
	return path
}

// StartAgent delegates to agentorch.Orchestrator and is exposed as a Wails-bound method.
// User-triggered starts are never one-shot — that flag is reserved for workflow
// steps that expect a single turn.
func (a *App) StartAgent(taskID, mode, prompt string, includeTaskDescription bool) (*agent.Agent, error) {
	return a.agentOrch.StartAgent(taskID, mode, prompt, includeTaskDescription, false)
}

// AgentQueueSnapshot exposes the read-only queue snapshot to Wails/web clients.
func (a *App) AgentQueueSnapshot() AgentQueueSnapshot {
	if a == nil || a.queueSvc == nil {
		return AgentQueueSnapshot{Items: []AgentQueueSnapshotItem{}}
	}
	return a.queueSvc.AgentQueueSnapshot()
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
