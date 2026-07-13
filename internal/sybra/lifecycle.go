package sybra

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autoupdate"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/confighot"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/selfmonitor"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/watchdog"
)

// LifecycleManager orchestrates background-service startup in discrete phases.
//
// Phase order (called from App.Startup after all init* methods complete):
//
//	lm.StartManagers(ctx, emit)  — watchdog, health, monitors, maintenance
//	lm.StartPollers(ctx, emit, issuesFetcher)  — pollers, orchestrator loop
//	lm.StartWatchers(ctx)  — config-file watcher
type LifecycleManager struct {
	app *App
}

func newLifecycleManager(app *App) *LifecycleManager {
	return &LifecycleManager{app: app}
}

// StartManagers launches watchdog, health-check, monitor/self-monitor services,
// maintenance loops, and OTel metrics observers.
func (lm *LifecycleManager) StartManagers(ctx context.Context, emit func(string, any)) {
	a := lm.app

	if a.cfg.Watchdog.Enabled {
		wdog := watchdog.New(a.agents, a.tasks, a.logger, emit, &a.wg, a.cfg.Watchdog, a.getPressureGate())
		a.wg.Go(func() { wdog.Run(ctx) })
	} else {
		a.logger.Info("watchdog.disabled")
	}

	hcheck := health.New(a.cfg.AuditDir(), a.tasks, config.HomeDir(), a.logger, emit, func() health.OwnedProcesses {
		owned := health.OwnedProcesses{
			PIDs:          map[int]bool{},
			ProcessGroups: map[int]bool{},
		}
		if pid := os.Getpid(); pid > 0 {
			owned.PIDs[pid] = true
		}
		for _, ag := range a.agents.ListAgents() {
			if ag == nil {
				continue
			}
			pid := ag.GetPID()
			if pid <= 0 {
				continue
			}
			owned.PIDs[pid] = true
			if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
				owned.ProcessGroups[pgid] = true
			}
		}
		return owned
	})
	a.wg.Go(func() { hcheck.Run(ctx) })

	lm.startMonitorService(ctx, emit)
	lm.startSelfMonitorService(ctx, emit)
	lm.startEvaluationService(ctx, emit)
	lm.startLearningDigestService(ctx, emit)
	lm.startPromptLabService(ctx, emit)
	lm.startAutoUpdate(ctx)
	lm.startAgentLogPruneLoop(ctx)
	lm.startTrashPruneLoop(ctx)
	lm.startTaskSnapshotLoop(ctx)
	lm.registerMetricsObservers()
}

// StartPollers launches task-source pollers and the orchestrator recovery loop.
func (lm *LifecycleManager) StartPollers(ctx context.Context, emit func(string, any), issuesFetcher *poll.IssuesFetcher) {
	a := lm.app
	a.wg.Go(func() { a.orchestratorLoop(ctx) })
	if a.mirror != nil {
		a.wg.Go(func() { a.mirror.Run(ctx) })
	}
	if a.clusterRoster != nil {
		a.wg.Go(func() { a.clusterHealthLoop(ctx) })
	}
	lm.startAppAuthLoop(ctx)
	lm.startRateBudgetLoop(ctx)
	lm.startPollHub(ctx, issuesFetcher)
	a.startTodoistLoop(ctx)
}

// appTokenRefreshInterval is how often the GitHub App installation token is
// renewed. Tokens last ~1h; refreshing well inside that keeps gh authenticated.
const appTokenRefreshInterval = 30 * time.Minute

// startAppAuthLoop enables GitHub App installation-token auth (when configured)
// and keeps the token fresh. With App auth on, every gh call runs against the
// installation token (15k/hr REST) instead of the personal token. A
// misconfiguration is logged and leaves gh on its own auth — never fatal.
func (lm *LifecycleManager) startAppAuthLoop(ctx context.Context) {
	a := lm.app
	app := a.cfg.GitHub.App
	if !app.Enabled {
		return
	}
	if err := github.EnableAppAuth(github.AppCredentials{
		AppID:          app.AppID,
		InstallationID: app.InstallationID,
		PrivateKeyPath: app.PrivateKeyPath,
	}); err != nil {
		a.logger.Error("github.app.disabled", "err", err)
		return
	}
	a.logger.Info("github.app.enabled", "app_id", app.AppID, "installation_id", app.InstallationID)
	a.wg.Go(func() {
		ticker := time.NewTicker(appTokenRefreshInterval)
		defer ticker.Stop()
		for {
			if err := github.RefreshAppToken(ctx); err != nil {
				a.logger.Warn("github.app.token.refresh", "err", err)
			} else if tok := github.CurrentAppToken(); tok != "" {
				_ = os.Setenv("GH_TOKEN", tok)
				_ = os.Setenv("GITHUB_TOKEN", tok)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// rateBudgetRefreshInterval is how often the free GET /rate_limit endpoint is
// polled to keep the request gate's budget view current for every gh path.
const rateBudgetRefreshInterval = 60 * time.Second

// startRateBudgetLoop periodically refreshes the shared GitHub rate-limit
// budget from the free /rate_limit endpoint, so the request gate and the
// global poll-interval throttle see accurate remaining quota across all gh
// subcommands — not only the `gh api --include` calls whose headers it can
// observe directly.
func (lm *LifecycleManager) startRateBudgetLoop(ctx context.Context) {
	a := lm.app
	if !runsGitHubRateBudgetLoop(a.cfg) {
		return
	}
	a.wg.Go(func() {
		ticker := time.NewTicker(rateBudgetRefreshInterval)
		defer ticker.Stop()
		for {
			if err := github.RefreshRateBudget(ctx); err != nil {
				a.logger.Debug("github.budget.refresh", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func runsGitHubRateBudgetLoop(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.GitHub.RunsReviewer() {
		return true
	}
	if !cfg.GitHub.RunsSearchPollers() {
		return false
	}
	return cfg.GitHub.RunsIssuesFetcher() || cfg.Renovate.Enabled
}

// StartWatchers launches the config-file hot-reload watcher.
func (lm *LifecycleManager) StartWatchers(ctx context.Context) {
	a := lm.app
	cfgPath := filepath.Join(config.HomeDir(), "config.yaml")
	cw := confighot.New(cfgPath, func() {
		changed, err := a.configSvc.ReloadFromDisk()
		if err != nil {
			a.logger.Error("config.reload.failed", "err", err)
			return
		}
		if len(changed) > 0 {
			a.logger.Info("config.reloaded", "changed", changed)
		}
	}, a.logger)
	if err := cw.Start(ctx); err != nil {
		a.logger.Error("config.watcher.start", "err", err)
		return
	}
	a.configWatcher = cw
}

func (lm *LifecycleManager) startAutoUpdate(ctx context.Context) {
	a := lm.app
	if !a.cfg.AutoUpdate.Enabled {
		return
	}
	repoDir := a.cfg.AutoUpdate.RepoDir
	if repoDir == "" {
		repoDir = a.repoDir
	}
	if repoDir == "" {
		a.logger.Error("autoupdate.disabled", "reason", "repo_dir is empty")
		return
	}
	if !filepath.IsAbs(repoDir) {
		a.logger.Error("autoupdate.disabled", "reason", "repo_dir is not absolute", "repo", repoDir)
		return
	}
	runner := autoupdate.New(autoupdate.Config{
		Enabled:        a.cfg.AutoUpdate.Enabled,
		RepoDir:        repoDir,
		Remote:         a.cfg.AutoUpdate.Remote,
		Branch:         a.cfg.AutoUpdate.Branch,
		Mode:           a.cfg.AutoUpdate.Mode,
		PollInterval:   time.Duration(a.cfg.AutoUpdate.PollSeconds) * time.Second,
		RequestRestart: a.requestRestart,
	}, a.logger)
	a.wg.Go(func() { runner.Run(ctx) })
}

// startAgentLogPruneLoop sweeps stale per-agent NDJSON files once a day.
// The startup call inside Recovery.RunStartupCleanup handles the first
// pass; this goroutine keeps the directory bounded on long-lived server
// deployments.
func (lm *LifecycleManager) startAgentLogPruneLoop(ctx context.Context) {
	a := lm.app
	a.wg.Go(func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lm.prune()
			}
		}
	})
}

// startTrashPruneLoop sweeps expired soft-deleted task generations once a
// day. The startup call inside Recovery.RunStartupCleanup handles the first
// pass; this goroutine bounds trash growth between restarts on long-lived
// server deployments, mirroring startAgentLogPruneLoop.
func (lm *LifecycleManager) startTrashPruneLoop(ctx context.Context) {
	a := lm.app
	a.wg.Go(func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.recovery.PruneTrash(ctx)
			}
		}
	})
}

// startTaskSnapshotLoop ensures the tasks-dir git snapshot repo exists,
// takes a baseline commit attempt so a fresh, non-empty tasks dir is
// captured immediately rather than waiting for the first ticker fire (a
// clean/empty tasks dir makes CommitNow a no-op, same as any other
// already-clean commit attempt), then launches the fixed-interval commit
// loop. Skips (with a warning) when TaskSnapshotEnabled is false or
// EnsureRepo could not make the snapshotter usable (git missing,
// corrupt/mismatched repo).
func (lm *LifecycleManager) startTaskSnapshotLoop(ctx context.Context) {
	a := lm.app
	if !a.cfg.TaskSnapshotEnabled() {
		a.logger.Info("tasksnapshot.disabled")
		return
	}
	if a.snapshotter == nil {
		a.logger.Warn("tasksnapshot.unavailable", "reason", "snapshotter not constructed")
		return
	}
	if !a.snapshotter.EnsureRepo(ctx) {
		a.logger.Warn("tasksnapshot.ensure_repo_failed")
		return
	}
	a.snapshotter.CommitNow(ctx)
	a.wg.Go(func() { a.snapshotter.Run(ctx) })
}

// prune is the periodic body of startAgentLogPruneLoop. Mirrors the
// retention computation Recovery does at startup so both passes use the
// same maxAge/gzipAfter/size cap and the same active-agent allowlist.
func (lm *LifecycleManager) prune() {
	a := lm.app
	var maxAge, gzipAfter time.Duration
	if days := a.cfg.DefaultLogRetentionDays(); days > 0 {
		maxAge = time.Duration(days) * 24 * time.Hour
	}
	if days := a.cfg.DefaultLogGzipAfterDays(); days > 0 {
		gzipAfter = time.Duration(days) * 24 * time.Hour
	}
	var maxTotalBytes int64
	if mb := a.cfg.DefaultLogRetentionMaxSizeMB(); mb > 0 {
		maxTotalBytes = int64(mb) * 1024 * 1024
	}
	r := logging.EnforceAgentLogRetention(a.logDir, logging.RetentionOptions{
		MaxAge:         maxAge,
		GzipAfter:      gzipAfter,
		MaxTotalBytes:  maxTotalBytes,
		ActiveLogPaths: a.agents.ActiveLogPaths(),
	}, time.Now())
	logging.LogPruneReport(a.logger, r)
}

// registerMetricsObservers wires OTel observable gauge callbacks to live
// subsystem state. No-op when metrics are disabled.
func (lm *LifecycleManager) registerMetricsObservers() {
	a := lm.app
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
	if fn := a.renovate.lastFetchedCount(); fn != nil {
		metrics.RegisterRenovatePRsFetched(fn)
	}
	if a.providerHealth != nil {
		metrics.RegisterProviderHealth(func() map[string]int64 {
			alertHealth, _ := providerHealthMetrics(a.providerHealth.Snapshot(), a.providerHealth.Failover)
			return alertHealth
		})
		metrics.RegisterProviderRawHealth(func() map[string]int64 {
			_, rawHealth := providerHealthMetrics(a.providerHealth.Snapshot(), a.providerHealth.Failover)
			return rawHealth
		})
	}
	metrics.RegisterAgentsInFlightByProvider(func() map[string]int64 {
		snapshot := a.agents.InFlightByProvider()
		out := make(map[string]int64, len(snapshot))
		for name, n := range snapshot {
			out[name] = int64(n)
		}
		return out
	})
}

func providerHealthMetrics(
	snapshot map[string]provider.Status,
	failover func(string) string,
) (alertHealth, rawHealth map[string]int64) {
	alertHealth = make(map[string]int64, len(snapshot))
	rawHealth = make(map[string]int64, len(snapshot))
	for name, s := range snapshot {
		if s.Healthy {
			alertHealth[name] = 1
			rawHealth[name] = 1
			continue
		}
		rawHealth[name] = 0
		if failover != nil && failover(name) != "" {
			alertHealth[name] = 1
		} else {
			alertHealth[name] = 0
		}
	}
	return alertHealth, rawHealth
}

// startMonitorService wires the in-process monitor loop when enabled.
func (lm *LifecycleManager) startMonitorService(ctx context.Context, emit func(string, any)) {
	a := lm.app
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
	// Work-Data Confidentiality: wrap the GH sink so anomalies on work-typed
	// tasks become scrubbed local sybra tasks instead of public issues. The
	// DowngradeLLMForTask closure forces work-typed LLM anomalies through the
	// deterministic path so they hit this sink (and get scrubbed) rather than
	// being dispatched to an agent that would file an issue itself.
	innerSink := monitor.NewGHIssueSink(a.cfg.Monitor.IssueLabel, a.cfg.Monitor.IssueRepo)
	// This callback's DispatchEvent -> execShell eventually derives its
	// context from workflow.Engine's own e.ctx field (Engine.SetContext),
	// not an explicit parameter threaded through the closure. contextcheck no
	// longer flags this call site (verified with a clean build+lint cache),
	// so no suppression directive is needed here.
	routingSink := newMonitorRoutingSink(innerSink, a.tasks, a.workScrubContextForTask, a.cfg.Monitor.IssueRepo, func(taskID string) {
		if a.workflowEngine == nil {
			return
		}
		a.wg.Go(func() {
			dispatched, err := a.workflowEngine.DispatchEvent(taskID, "task.created", nil, nil)
			if err != nil {
				a.logger.Error("monitor.routing.workflow.failed", "task_id", taskID, "err", err)
				return
			}
			if dispatched == "" {
				a.logger.Warn("monitor.routing.workflow.no-match", "task_id", taskID)
			}
		})
	}, a.logger)
	svc := monitor.NewService(monitor.Deps{
		Cfg:        a.cfg.Monitor,
		Tasks:      a.tasks,
		Audit:      monitor.AuditDirReader(a.cfg.AuditDir()),
		Agents:     a.agents,
		Dispatcher: disp,
		Sink:       routingSink,
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
		DowngradeLLMForTask: func(taskID string) bool {
			t, err := a.tasks.Get(taskID)
			if err != nil {
				return false
			}
			return a.workScrubContextForTask(t.ProjectID) != nil
		},
		RecoverLostAgent: func(ctx context.Context) {
			if a.recovery == nil {
				return
			}
			// Keep the monitor tick responsive: the stale-agent sweep can scan
			// all in-progress tasks and spawn recovery work.
			a.wg.Go(func() {
				a.recovery.RestartStaleInProgress(ctx)
			})
		},
	})
	a.monitorSvc = svc
	a.wg.Go(func() { svc.Run(ctx) })
}

// startSelfMonitorService wires the deep-analysis loop that distills agent logs
// via loganalyzer and persists a Report to ~/.sybra/selfmonitor/last-report.json.
func (lm *LifecycleManager) startSelfMonitorService(ctx context.Context, emit func(string, any)) {
	a := lm.app
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
			Gate:   a.providerHealth,
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

// startEvaluationService constructs the read-only scorecard service and launches
// its ticker. The service is built even when disabled so GetEvaluationReport can
// still compute on demand for the dashboard; Run() no-ops when not enabled.
func (lm *LifecycleManager) startEvaluationService(ctx context.Context, emit func(string, any)) {
	a := lm.app
	deps := evaluation.Deps{
		Cfg:        a.cfg.Evaluation,
		ABTesting:  a.cfg.ABTesting,
		Audit:      evaluation.AuditDirReader(a.cfg.AuditDir()),
		Emit:       emit,
		Logger:     a.logger,
		ReportPath: config.EvaluationReportPath(),
	}
	if a.stats != nil {
		deps.Stats = a.stats
	}
	svc := evaluation.NewService(deps)
	a.evaluationSvc = svc
	a.wg.Go(func() { svc.Run(ctx) })
}

// startLearningDigestService constructs the periodic Learning Digest service
// and launches its ticker. The service is built even when disabled so
// RunLearningDigestNow/GetLearningDigestStatus still work on demand; Run()
// no-ops when not enabled (mirroring startEvaluationService).
func (lm *LifecycleManager) startLearningDigestService(ctx context.Context, emit func(string, any)) {
	a := lm.app
	deps := learning.Deps{
		Cfg:       a.cfg.LearningDigest,
		ABTesting: a.cfg.ABTesting,
		Audit:     learning.AuditDirReader(a.cfg.AuditDir()),
		AuditLog:  a.audit,
		Store:     a.learning,
		Blocklist: a.fleetWorkBlocklist,
		Emit:      emit,
		Logger:    a.logger,
	}
	if a.stats != nil {
		deps.Stats = a.stats
	}
	// a.providerHealth is a typed *provider.Checker that stays nil when
	// health-checking is disabled; assigning a nil pointer straight into the
	// Gate interface field would produce a non-nil interface wrapping a nil
	// receiver, and Checker's methods are not nil-receiver-safe.
	if a.providerHealth != nil {
		deps.Gate = a.providerHealth
	}
	svc := learning.NewService(deps)
	a.learningDigestSvc = svc
	a.wg.Go(func() { svc.Run(ctx) })
}

// startPromptLabService launches the deterministic (non-LLM) Prompt Lab
// ticker built by initPromptLab. No-op when the coordinator is nil (e.g.
// tests or other callers that exercise LifecycleManager without going
// through App.Startup's initAutomations step) or cfg.PromptLab.Enabled is
// false — the disabled check lives inside promptLabCoordinator.run so this
// stays a thin launch point, consistent with the other startX helpers in
// this file.
func (lm *LifecycleManager) startPromptLabService(ctx context.Context, _ func(string, any)) {
	a := lm.app
	if a.promptLab == nil {
		return
	}
	a.wg.Go(func() { a.promptLab.run(ctx) })
}

// pollRegistrar is the subset of *poll.Hub used by registerPollHandlers,
// extracted so tests can substitute a fake and assert on which fetchers were
// registered without inspecting poll.Hub's private fields.
type pollRegistrar interface {
	Register(f poll.Fetcher, initialWait time.Duration)
}

// registerPollHandlers registers all enabled poll handlers onto reg. Split
// out of startPollHub so tests can exercise the registration logic (e.g.
// reviewer gating) directly against a fake pollRegistrar.
func registerPollHandlers(a *App, reg pollRegistrar, issuesFetcher *poll.IssuesFetcher) {
	if a.cfg.IsFollower() {
		a.logger.Info("cluster.follower.pollers.disabled", "reason", "role=follower; leader owns task-source polling")
		return
	}
	// Periodic GitHub search pollers (reviews/issues/renovate) only run on the
	// primary instance — a "secondary" machine sharing the same token skips them
	// so the shared rate budget isn't billed twice. Triage is local (no GitHub
	// search) and always runs.
	runSearch := a.cfg.GitHub.RunsSearchPollers()
	if !runSearch {
		a.logger.Info("github.pollers.secondary", "reason", "poller_role=secondary; skipping reviews/issues/renovate searches")
	}
	if a.reviewer != nil {
		if a.cfg.GitHub.RunsReviewer() {
			reg.Register(a.reviewer, 10*time.Second)
		} else {
			a.logger.Info("github.reviews.disabled",
				"github_enabled", a.cfg.GitHub.Enabled,
				"reviews_enabled", a.cfg.GitHub.ReviewsEnabled,
			)
		}
	}
	if runSearch {
		if issuesFetcher != nil {
			reg.Register(issuesFetcher, 20*time.Second)
		}
		if renovatePoller := a.renovate.poller(); renovatePoller != nil {
			reg.Register(renovatePoller, 15*time.Second)
		}
	}
	if triagePoller := a.triage.poller(); triagePoller != nil {
		reg.Register(triagePoller, 30*time.Second)
	}
}

// startPollHub registers all enabled poll handlers and starts the hub.
func (lm *LifecycleManager) startPollHub(ctx context.Context, issuesFetcher *poll.IssuesFetcher) {
	a := lm.app
	hub := poll.NewHub()
	registerPollHandlers(a, hub, issuesFetcher)
	metrics.RegisterPollerAuthHealth(hub.AuthHealthSnapshot)
	hub.Start(ctx, &a.wg, a.logger)
}
