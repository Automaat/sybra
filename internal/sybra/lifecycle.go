package sybra

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/confighot"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
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

	wdog := watchdog.New(a.agents, a.tasks, a.logger, emit, &a.wg)
	a.wg.Go(func() { wdog.Run(ctx) })

	hcheck := health.New(a.cfg.AuditDir(), a.tasks, config.HomeDir(), a.logger, emit)
	a.wg.Go(func() { hcheck.Run(ctx) })

	lm.startMonitorService(ctx, emit)
	lm.startSelfMonitorService(ctx, emit)
	lm.startAgentLogPruneLoop(ctx)
	lm.registerMetricsObservers()
}

// StartPollers launches task-source pollers and the orchestrator recovery loop.
func (lm *LifecycleManager) StartPollers(ctx context.Context, emit func(string, any), issuesFetcher *poll.IssuesFetcher) {
	a := lm.app
	a.wg.Go(func() { a.orchestratorLoop(ctx) })
	lm.startPollHub(ctx, issuesFetcher)
	a.startTodoistLoop(ctx)
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

// prune is the periodic body of startAgentLogPruneLoop. Mirrors the
// retention computation Recovery does at startup so both passes use the
// same maxAge.
func (lm *LifecycleManager) prune() {
	a := lm.app
	var maxAge time.Duration
	if days := a.cfg.DefaultLogRetentionDays(); days > 0 {
		maxAge = time.Duration(days) * 24 * time.Hour
	}
	r := logging.PruneAgentLogs(a.logDir, maxAge, time.Now())
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
			// Monitor files anomaly issues on Monitor.IssueRepo (defaults to
			// Automaat/sybra). Work-typed projects must never surface there —
			// anomaly bodies carry task IDs and audit excerpts that may
			// reference work-repo content. See CLAUDE.md — Work-Data
			// Confidentiality.
			if p.Type == project.ProjectTypeWork {
				return false
			}
			return a.allowsProjectType(p.Type)
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

// startPollHub registers all enabled poll handlers and starts the hub.
func (lm *LifecycleManager) startPollHub(ctx context.Context, issuesFetcher *poll.IssuesFetcher) {
	a := lm.app
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
