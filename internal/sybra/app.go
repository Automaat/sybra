package sybra

// Dependency graph (startup order):
//
//	config → audit/stats → task.Store → project.Store → loopagent.Store
//	→ emit/bgops → task.Manager → limits/approval → agent.Manager → providerHealth
//	→ worktrees → sandboxes → agentOrch → reviewer → workflowEngine
//	→ wireServices → mintAppTokenBeforeRecovery → RunStartupCleanup
//	→ [LifecycleManager: StartManagers → StartPollers → StartWatchers]

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
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/confighot"
	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/diskreclaim"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/intervention"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/pressure"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/routing"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/selfmonitor"
	"github.com/Automaat/sybra/internal/spotlight"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/sybra/completion"
	"github.com/Automaat/sybra/internal/sybra/dispatch"
	"github.com/Automaat/sybra/internal/sybra/reconciliation"
	"github.com/Automaat/sybra/internal/sybra/review"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/sybra/verification"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/tasksnapshot"
	"github.com/Automaat/sybra/internal/toolledger"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/google/uuid"
)

type App struct {
	ctx             context.Context
	cancel          context.CancelFunc
	schedulerCtx    context.Context
	schedulerCancel context.CancelFunc
	watcherCtx      context.Context
	watcherCancel   context.CancelFunc
	lifecycle       atomic.Uint32
	backgroundMu    sync.Mutex // serializes tracked background work with drain
	wg              sync.WaitGroup
	// fetchPRHeadSHA overrides the PR-head lookup in tests; nil uses GitHub.
	fetchPRHeadSHA func(ctx context.Context, repo string, number int) (string, error)
	fetchPR        func(ctx context.Context, repo string, number int) (github.PullRequest, error)
	tasks          *task.Manager
	projects       *project.Store
	database       *db.DB
	loopAgents     loopagent.Repository
	loopSched      *loopagent.Scheduler
	agents         *agent.Manager
	// boardTarget/boardToken/boardCA name the board task-scoped agents reach,
	// held here because SetAgentBoard runs before Startup builds the manager.
	boardTarget       string
	boardToken        string
	boardCA           string
	attempts          *dispatch.Controller
	watcher           *watcher.Watcher
	configWatcher     *confighot.Watcher
	notifier          *notification.Emitter
	audit             *audit.Logger
	attachments       *attachment.Store
	artifacts         *artifact.Store
	evidenceStore     *evidence.Store
	experience        experience.Repository
	intervention      *intervention.Store
	cleanupProtected  *cleanup.ProtectedStore
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
	routingSvc        *routing.Service
	agentOrch         *agentorch.Orchestrator
	runenv            *runenv.Service
	verification      *verification.Manager
	postRunReconciler *reconciliation.Reconciler
	reviewer          *review.Handler
	assigner          *clusterlead.Assigner
	mirror            *clusterlead.Mirror
	clusterRoster     *cluster.Roster
	clusterSvc        *ClusterService
	workflowEngine    *workflow.Engine
	workflowStore     workflow.Repository
	pressureGate      *pressure.Gate
	diskReclaimer     *diskreclaim.Reclaimer
	toolLedger        *toolledger.Logger
	renovate          *renovateCoordinator
	promptLab         *promptLabCoordinator
	triage            *triageCoordinator
	humanReview       *humanReviewHandler
	// activeCfg is the immutable configuration snapshot used by concurrent
	// runtime readers. cfg remains the construction-time snapshot for legacy
	// startup wiring and tests; hot reloads must never mutate it in place.
	activeCfg       atomic.Pointer[config.Config]
	cfg             *config.Config
	baseABTesting   atomic.Value
	liveABTesting   atomic.Value
	logLevel        *slog.LevelVar
	emit            func(string, any)
	emitFactory     func(context.Context) func(string, any)
	openBrowser     func(string)
	requestRestart  func()
	restartStaleErr *logging.ErrorThrottle
	// dispatchNudge wakes the orchestrator dispatch pass on demand (e.g. on a
	// status change) so a freshly-ready task isn't left idle until the next
	// fast tick. Buffered, size 1, coalescing — see nudgeDispatch.
	dispatchNudge chan struct{}
	// schedulerDisabled and brainDisabled are the resolved instance-role gates,
	// sampled once by applyInstanceRole so the orchestrator loop never races a
	// config reload rewriting cfg.Orchestrator in place. Stored negated so the
	// zero value means "full" — an App built without applyInstanceRole (tests,
	// future call sites) keeps the behavior it had before the role key existed
	// rather than silently refusing to dispatch.
	schedulerDisabled atomic.Bool
	brainDisabled     atomic.Bool
	// startupRecoveryPending is set true just before initStatusHook (the
	// earliest dispatch observer to be wired) and stays true until
	// RunStartupCleanup (reattach survivor agents, replay persisted effects,
	// restart stale runs) finishes. The status-change hook and, later, the file
	// watcher go live before that reattach completes, so dispatchTaskCreatedWorkflow,
	// dispatchPlanningWorkflow, dispatchStatusWorkflow and dispatchInboundReviewWorkflow
	// — the sinks that auto-start work — refuse to dispatch while this is set: until
	// reattach runs, HasRunningAgentForTask reads an empty registry and an
	// early dispatch could start a duplicate agent on a live worktree
	// (#2752). Zero value (unset) reports "not pending", so an App built
	// without going through Startup (tests, direct construction) dispatches
	// exactly as it did before this gate existed. Cleared once
	// RunStartupCleanup returns, followed by replayDeferredStatusChanges (for
	// the workflow status events the window suppressed) and a nudgeDispatch so
	// any task that changed status during the window gets picked up immediately
	// by the board-wide reconcileRunnableBoardTasks sweep instead of waiting for
	// the next dispatch tick.
	startupRecoveryPending atomic.Bool
	// deferredStatusChanges buffers the tasks whose status change was observed
	// while startupRecoveryPending was set, so the suppressed
	// workflowEngine.HandleStatusChange can be replayed once reattach finishes.
	// Without the replay a run_agent step parked on wait_for_status never sees
	// its awaited status again: agent completion deliberately does not advance
	// such a step, board reconciliation skips tasks with an active workflow, and
	// ResumeStalled re-runs the agent instead of comparing the persisted status
	// to WaitForStatus. A set, not a queue — replay reads each task's current
	// persisted status, which coalesces several transitions in the window down
	// to the one that is still true.
	deferredStatusMu      sync.Mutex
	deferredStatusChanges map[string]struct{}
	// maintenanceCleanupRunning prevents slow git cleanup from stacking across
	// maintenance ticks. Cleanup itself runs outside the orchestrator loop.
	maintenanceCleanupRunning atomic.Bool
	worktreeCleanupFn         func(context.Context) // test seam; nil uses worktrees
	// recoveryStartGate, if non-nil, is closed by the caller to release the
	// recovery goroutine right before it calls RunStartupCleanup. Test seam
	// only: it lets a test observe startupRecoveryPending's armed state with a
	// guaranteed happens-before instead of racing the goroutine's own
	// scheduling (Go gives no ordering guarantee between a freshly spawned
	// goroutine and the code after the spawning call returns).
	recoveryStartGate chan struct{}
	recovery          *recovery.Recovery
	snapshotter       *tasksnapshot.Snapshotter
	agentCompletion   *completion.Handler
	// umbrellaCloseIssue closes the umbrella GitHub issue on full roll-up.
	// nil defaults to github.CloseIssue; overridden in tests.
	umbrellaCloseIssue func(repo string, number int, comment string) error
	// umbrellaFetchIssue fetches a dependency's closing issue to check a
	// "label" DepCondition. nil defaults to github.FetchIssue; overridden in
	// tests.
	umbrellaFetchIssue func(repo string, number int) (github.Issue, error)

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
	auditSvc     *AuditService
	selfMonSvc   *SelfMonitorService
	reviewSvc    *ReviewService
	workflowSvc  *WorkflowService
	infoSvc      *InfoService
	browserSvc   *BrowserService
	learningSvc  *LearningService
	promptLabSvc *PromptLabService

	// HTTP-only services. QueueService must stay out of V3Services().
	queueSvc *QueueService
}

// goWhileRunning adds tracked background work only while shutdown has not
// begun. BeginDrain holds the same lock before it transitions lifecycle state,
// closing the WaitGroup Add-vs-Wait window during Shutdown.
func (a *App) goWhileRunning(fn func()) bool {
	if a == nil || fn == nil {
		return false
	}
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	switch a.lifecycleState() {
	case lifecycleStateIdle, lifecycleStateRunning:
		// Tests and startup code may schedule work before the running marker is
		// installed; both states are safe until BeginDrain takes backgroundMu.
	case lifecycleStateDraining, lifecycleStateStopping, lifecycleStateStopped:
		return false
	}
	a.wg.Go(fn)
	return true
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
	a.activeCfg.Store(cfg)
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
	a.auditSvc = &AuditService{}
	a.selfMonSvc = &SelfMonitorService{}
	a.reviewSvc = &ReviewService{}
	a.workflowSvc = &WorkflowService{}
	a.infoSvc = &InfoService{}
	a.browserSvc = &BrowserService{}
	a.learningSvc = &LearningService{}
	a.promptLabSvc = &PromptLabService{}
	a.queueSvc = &QueueService{}
	a.clusterSvc = &ClusterService{logger: logger}
	for _, o := range opts {
		o(a)
	}
	a.initializeABTesting(cfg.ABTesting)
	return a
}

// currentConfig returns one immutable configuration snapshot for the caller's
// whole operation. A reload publishes a replacement snapshot atomically, so a
// reader can never observe a torn struct or a mixture of two configurations.
func (a *App) currentConfig() *config.Config {
	if a == nil {
		return nil
	}
	if cfg := a.activeCfg.Load(); cfg != nil {
		return cfg
	}
	return a.cfg
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

func (a *App) startLifecycle(schedulerCtx, watcherCtx context.Context, emit func(string, any)) {
	a.applyInstanceRole()
	a.initLoopScheduler(schedulerCtx, emit)
	a.initFileWatcher(watcherCtx, emit)

	issuesFetcher := a.initAutomations(schedulerCtx, emit)
	a.wireServices(emit) //nolint:contextcheck // TaskService uses the app-bound root context; see Startup's contextcheck note.

	// syncSkillsBundle's deep diagnostic logging uses context.Background()
	// intentionally (see skillsync.Syncer.log) — not a cancellation bug.
	a.syncSkillsBundle(project.NormalizeSigningPolicy(a.cfg.CommitSigning())) //nolint:contextcheck // plain diagnostic logging inside skillsync, see its log() comment
	a.snapshotter = tasksnapshot.New(config.TaskSnapshotGitDir(), a.tasksDir, time.Duration(a.cfg.DefaultTaskSnapshotInterval())*time.Second, a.logger)
	// EnsureRepo must run before RunStartupCleanup: the startup trash prune
	// fires CommitBeforePrune, which on a fresh install would otherwise commit
	// into an uninitialized git dir and fail silently. StartManagers'
	// startTaskSnapshotLoop calls EnsureRepo again (idempotent).
	if a.cfg.TaskSnapshotEnabled() {
		a.snapshotter.EnsureRepo(schedulerCtx)
	}
	a.recovery = a.newRecovery()
	a.RegisterSpotlightHotkey() //nolint:contextcheck // agent.Manager dispatch chain uses its own m.ctx field, see Startup's contextcheck note

	lm := newLifecycleManager(a)
	// Routing reads the evaluation service's cached report on its own
	// goroutine, so the service pointer must be published before routing
	// primes or starts ticking.
	lm.startEvaluationService(schedulerCtx, emit)
	// Routing must prime before Startup returns; otherwise the first workflow
	// dispatch after a fresh enabled boot can beat version 1 publication.
	lm.startRoutingService(schedulerCtx, emit)
	lm.StartWatchers(schedulerCtx)
	// Mint the GitHub App installation token synchronously (bounded timeout)
	// before RunStartupCleanup's recovery pushes and the monitor's
	// Authenticated() preflight run — both used to race an empty token on
	// every boot, since the mint previously lived inside StartPollers,
	// several steps after recovery already ran. A mint outage degrades to
	// ambient gh credentials instead of blocking startup. See #2494.
	lm.mintAppTokenBeforeRecovery(schedulerCtx)

	a.wg.Go(func() {
		if a.recoveryStartGate != nil {
			<-a.recoveryStartGate
		}
		a.recovery.RunStartupCleanup(schedulerCtx)
		if a.verification != nil && a.agents != nil {
			active := make(map[string]struct{})
			for _, ag := range a.agents.ListLiveAgents() {
				active[ag.ID] = struct{}{}
			}
			a.verification.Reconcile(active)
		}
		// Startup cleanup reattaches surviving provider processes before replay
		// can call the synchronous human-required dispatch path. Running this
		// earlier sees an empty live-agent registry and can start a duplicate.
		if a.humanReview != nil {
			a.humanReview.recoverStrandedUnblockedTasks() //nolint:contextcheck // handler bounds its git/PR checks with dedicated timeouts, matching live completion.
		}
		a.certifyStartupRunEnvironment(schedulerCtx)
		// Arm dispatch now that reattach/replay/restart-stale have run, then
		// nudge so any task that changed status during the window (buffered,
		// not dispatched — see startupRecoveryPending) is picked up by the
		// board-wide reconcile sweep right away instead of the next tick.
		a.startupRecoveryPending.Store(false)
		// Replay before the nudge: board reconciliation skips a task whose
		// workflow is still active, so a step parked on wait_for_status has to
		// be advanced by the deferred event itself, not by the sweep.
		a.replayDeferredStatusChanges() //nolint:contextcheck // same engine chain as initStatusHook, which binds its own e.ctx; see Startup's contextcheck note
		a.nudgeDispatch()
		// After the nudge, never before it: this sweeps parks whose review
		// spawn a previous shutdown dropped, and each one prepares a worktree.
		// Ahead of arming, it delays every live task by its own setup time.
		if a.humanReview != nil {
			go a.humanReview.RespawnDroppedReviews(schedulerCtx)
		}
		lm.StartManagers(schedulerCtx, emit)
		lm.StartPollers(schedulerCtx, emit, issuesFetcher)
	})
}

func (a *App) cleanupFailedStartup() {
	if a.cancel != nil {
		a.cancel()
	}
	a.stopFileWatcher()
	a.closeDatabase()
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

	appCtx, schedulerCtx, watcherCtx := a.initLifecycle(ctx)
	a.logger.Info("app.starting")

	a.initAudit()
	a.initToolLedger()
	a.initStats()

	if err := a.initDatabase(appCtx); err != nil {
		return err
	}

	store, err := task.NewStore(a.tasksDir)
	if err != nil {
		a.logger.Error("task.store.init", "err", err)
		return fmt.Errorf("task store: %w", err)
	}
	// Eagerly run the legacy-field backfill (see task.Store.Migrate) so
	// startup pays that cost once instead of leaving it to the first List()
	// caller. List() still self-heals on read regardless, so a failure here
	// is logged rather than fatal.
	if err := store.Migrate(); err != nil {
		a.logger.Warn("task.store.migrate", "err", err)
	}

	if err := a.initProjects(appCtx); err != nil {
		return err
	}

	if err := a.initLoopAgents(appCtx); err != nil {
		return fmt.Errorf("loop agents: %w", err)
	}
	if a.emitFactory != nil {
		a.emit = a.emitFactory(appCtx)
	} else {
		a.emit = func(string, any) {}
	}
	emit := a.taskEventEmitter(store)
	a.initBgops(appCtx, emit)

	a.emitDegradedWarnings(emit)
	a.tasks = task.NewManager(store, task.EmitterFunc(emit))
	// Arm the dispatch gate before the status hook is wired — initStatusHook's
	// handler reaches dispatchStatusWorkflow/dispatchTaskCreatedWorkflow, and
	// the file watcher (initFileWatcher, later in startLifecycle) also observes
	// status changes. atomic.Bool's zero value is false ("not pending"), so the
	// gate would fail open for any init step in this window that flips a task's
	// status. Set it here, before either observer exists. Cleared only after
	// RunStartupCleanup's reattach populates the live agent registry (see
	// startLifecycle and startupRecoveryPending's doc comment).
	a.startupRecoveryPending.Store(true)
	a.initStatusHook() //nolint:contextcheck // workflow engine uses its own e.ctx field, see Startup's contextcheck note
	a.initLocalStores(appCtx)
	a.cleanupProtected = cleanup.DefaultProtectedStore()
	a.notifier = notification.New(emit)
	a.notifier.SetDesktop(a.cfg.Notification.Desktop)
	a.initLimits() //nolint:contextcheck // backfill derives from a.ctx directly, see Startup's contextcheck note
	// initSandboxes has no agent-manager dependency and must run before
	// initAgentManager so ManagerConfig.SandboxHome can be wired at construction.
	a.initSandboxes()
	if err := a.initAgentManager(appCtx, emit); err != nil {
		return err
	}
	a.initProviderHealth(schedulerCtx, emit)

	a.prTracker = github.NewIssueTrackerWithMaxRetries(30*time.Minute, a.cfg.GitHub.PRFixRetries())

	configureProjectGitDefaults(a.worktreesDir)
	// Initialize domain services (dependency order: worktrees → agentOrch → reviewer, workflow)
	a.worktrees = worktree.New(worktree.Config{
		WorktreesDir:      a.worktreesDir,
		Projects:          a.projects,
		Tasks:             a.tasks,
		Logger:            a.logger,
		LogsDir:           a.logDir,
		PRBranchResolver:  github.FetchPRBranch,
		AgentChecker:      a.agents.HasRunningAgentForTask,
		LiveAgentChecker:  a.agents.HasLiveRegisteredAgentForTask,
		ProtectedFindings: a.cleanupProtected,
	})
	a.agentOrch = agentorch.New(a.tasks, a.projects, a.agents, a.audit, a.logger, a.worktrees, a.cfg)
	a.agentOrch.SetContext(appCtx)
	a.initRunEnvironment()
	a.initReviewer(emit)

	wfStore, wfErr := a.openWorkflowStore(appCtx)
	if wfErr != nil {
		a.logger.Error("workflow.store.init", "err", wfErr)
	}
	a.initWorkflowEngine(wfStore)

	a.initCluster()

	a.initAgentConfig()

	a.startLifecycle(schedulerCtx, watcherCtx, emit)

	a.logAutomationsSummary()
	a.logger.Info("app.started")
	started = true
	return nil
}

func (a *App) initReviewer(emit func(string, any)) {
	a.reviewer = review.New(a.tasks, a.projects, a.agents, a.audit, a.logger, a.prTracker, emit, a.worktrees, a.renovatePRsForMonitor, a.cfg, a.experience)
	a.reviewer.SetABTestingSource(a.abTestingConfig)
	a.reviewer.SetInterventionStore(a.intervention)
	a.reviewer.SetVerification(a.verification)
}

func (a *App) taskEventEmitter(store *task.Store) func(string, any) {
	return func(event string, data any) {
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
}

func configureProjectGitDefaults(worktreesDir string) {
	project.FetchTTL = 60 * time.Second
	project.QuarantineDir = filepath.Join(config.HomeDir(), "quarantine")
	project.WorktreesDir = worktreesDir
}

// sandboxRetentionWindow translates the resolved sandbox.retention_hours
// config into the sentinel sandbox.Manager.SetRetentionWindow expects:
// a negative duration disables age-based pruning.
func sandboxRetentionWindow(cfg *config.Config) time.Duration {
	if window, disabled := cfg.DefaultSandboxRetention(); !disabled {
		return window
	}
	return -1
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

// GetAutonomyTrend returns all-time / last-week / last-month autonomy
// snapshots plus a week-by-week trend, so the Evaluation tab can show how
// autonomy has moved over time instead of only the current rolling window.
func (a *App) GetAutonomyTrend() evaluation.AutonomyTrend {
	if a.evaluationSvc == nil {
		return evaluation.AutonomyTrend{}
	}
	trend, err := a.evaluationSvc.AutonomyTrend(context.Background())
	if err != nil {
		a.logger.Warn("evaluation.autonomy_trend.failed", "err", err)
		return evaluation.AutonomyTrend{}
	}
	return trend
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

func (a *App) Shutdown(ctx context.Context) {
	a.BeginDrain()
	if a.loopSched != nil {
		a.loopSched.Stop()
	}
	waitCtx, cancel := shutdownWaitContext(ctx)
	defer cancel()
	if !waitGroupContext(waitCtx, &a.wg) {
		a.logger.Warn("app.shutdown.wait_timeout", "grace", shutdownWaitBudget(waitCtx), "stacks", a.dumpGoroutineStacks())
	}
	a.logger.Info("app.stopping")
	a.beginShutdown()
	if a.agents != nil {
		a.agents.Shutdown()
	}
	a.stopFileWatcher()
	if a.audit != nil {
		_ = a.audit.Close()
	}
	if a.toolLedger != nil {
		_ = a.toolLedger.Close()
	}
	a.closeDatabase()
	if a.homeUnlock != nil {
		if err := a.homeUnlock(); err != nil {
			a.logger.Warn("app.home_unlock.failed", "err", err)
		}
		a.homeUnlock = nil
	}
	a.finishShutdown()
	a.logger.Info("app.stopped")
}

const appShutdownWaitGrace = 15 * time.Second
const fileWatcherShutdownGrace = 2 * time.Second

func shutdownWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, appShutdownWaitGrace)
}

func shutdownWaitBudget(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return appShutdownWaitGrace
}

func waitGroupContext(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *App) stopFileWatcher() {
	cancel := a.watcherCancel
	a.watcherCancel = nil
	if cancel == nil {
		return
	}
	cancel()
	if a.watcher == nil {
		return
	}
	select {
	case <-a.watcher.Done():
	case <-time.After(fileWatcherShutdownGrace):
		a.logger.Warn("watcher.shutdown.timeout", "grace", fileWatcherShutdownGrace)
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

// StartK8sPocAgent starts a project-less headless run directly through
// agent.Manager. It exists to smoke-test the experimental Kubernetes Job runner
// without requiring a project/worktree. Normal production dispatch should keep
// using StartAgent/workflows.
func (a *App) StartK8sPocAgent(prompt string) (*agent.Agent, error) {
	if a == nil || a.agents == nil {
		return nil, fmt.Errorf("agent manager unavailable")
	}
	if a.cfg == nil || !a.cfg.Agent.K8sJobs.Enabled {
		return nil, fmt.Errorf("kubernetes job runner is not enabled")
	}
	home, err := os.MkdirTemp("", "sybra-k8s-poc-*")
	if err != nil {
		return nil, fmt.Errorf("create k8s poc home: %w", err)
	}
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:   "k8s-poc-" + uuid.NewString()[:8],
		Name:     string(agent.RoleImplementation),
		Role:     agent.RoleImplementation,
		Mode:     "headless",
		Prompt:   prompt,
		Dir:      home,
		ExtraEnv: []string{"SYBRA_HOME=" + home, "SYBRA_CONTROL_HOME=" + home},
	})
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	return ag, nil
}

// AgentQueueSnapshot exposes the read-only queue snapshot to Wails/web clients.
func (a *App) AgentQueueSnapshot() AgentQueueSnapshot {
	if a == nil || a.queueSvc == nil {
		return AgentQueueSnapshot{Items: []AgentQueueSnapshotItem{}}
	}
	return a.queueSvc.AgentQueueSnapshot()
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

	a.registerSpotlight(func() {
		projectsJSON := "[]"
		if projects, err := a.projectSvc.ListProjects(); err == nil {
			if data, err := json.Marshal(projects); err == nil {
				projectsJSON = string(data)
			}
		}
		spotlight.ShowPanel(projectsJSON)
	})
}

// Context returns the app's running context.
func (a *App) Context() context.Context { return a.ctx }

// HTTPAdmission decides whether one HTTP API method may run.
func (a *App) HTTPAdmission(service, method string, meta httpapi.MethodMeta) error {
	return a.httpAdmission(service, method, meta)
}

// SetAgentBoard tells task-scoped agents which board to reach.
//
// An agent's sybra-cli has no filesystem path to task state and cannot
// discover a board from inside the process sandbox, so it is given the address
// instead. Call this before Startup: the recovery pass dispatches agents for
// runs it finds stale, and one that starts unnamed burns a whole run on CLI
// calls that all refuse. The address comes from configuration, so it is known
// before anything listens.
func (a *App) SetAgentBoard(target, token, ca string) {
	if a == nil {
		return
	}
	a.boardTarget, a.boardToken, a.boardCA = target, token, ca
	// Startup has not run yet in the ordinary case; initAgents applies it to
	// the manager it builds. Applied here too so a later call still lands.
	if a.agents != nil {
		a.agents.SetBoard(target, token, ca)
	}
}
